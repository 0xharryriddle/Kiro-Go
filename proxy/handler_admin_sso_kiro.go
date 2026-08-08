package proxy

import (
	"encoding/json"
	"errors"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}
func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Best-effort identity at login time. For an IAM Identity Center tenant whose
	// CodeWhisperer profile lives outside the SSO portal region this call cannot
	// succeed yet (no profileArn, wrong region), so a blank result is expected and
	// is repaired by hydrateAccountAfterLogin below.
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Resolve the profile across regions and backfill identity now that the
	// account is persisted, so the admin UI shows the real account and profile
	// instead of an unidentified row with no profile.
	hydrateAccountAfterLogin(&account)

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}
func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}
func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiStartKiroSso starts the Kiro hosted-portal sign-in (Enterprise SSO — Microsoft 365 /
// Entra ID, plus Google/GitHub). It binds the loopback callback listener and returns the
// sign-in URL the operator opens in a browser ON THE SAME HOST as the proxy (the OAuth
// redirect targets 127.0.0.1:3128). The browser is driven through the enterprise external-IdP
// leg automatically; the front end polls /auth/kiro-sso/poll until completion.
func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	// Region is optional (defaults to us-east-1 in StartKiroSsoLogin), so a decode
	// error (including an empty body) is intentionally tolerated — mirrors
	// apiStartBuilderIdLogin.
	json.NewDecoder(r.Body).Decode(&req)

	session, signInURL, err := auth.StartKiroSsoLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

// apiCancelKiroSso tears down an in-flight hosted-portal sign-in (operator closed or
// cancelled the modal), freeing the loopback callback port immediately instead of
// waiting for the deadline. It also drops any tokens parked awaiting a profile
// choice, so a dismissed picker doesn't leave credentials in memory for the TTL.
func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		h.kiroSsoLifecycleMu.Lock()
		defer h.kiroSsoLifecycleMu.Unlock()
		auth.CancelKiroSsoLogin(req.SessionID)
		// There are TWO independent parking mechanisms for a credential awaiting a
		// profile choice, and a cancel must clear BOTH or the one left behind keeps
		// tokens in memory for its full TTL:
		//   - kiroSsoProfileChoiceStore backs /auth/kiro-sso/profile
		//     (kiro_sso_profile_admin.go).
		//   - pendingKiroSsoChoices backs /auth/kiro-sso/select-profile (below).
		// Both are no-ops when the session parked nothing.
		h.getKiroSsoProfileChoiceStore().cancel(req.SessionID)
		dropPendingKiroSsoChoice(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// --- Deferred profile choice (external_idp multi-region) ---------------------
//
// When the eager probe finds 2+ Kiro profiles for a freshly-exchanged
// external_idp credential, the account is NOT created yet: the exchanged tokens
// and the discovered profile list are parked here (keyed by the SSO session id)
// until the operator picks a profile via /auth/kiro-sso/select-profile, cancels,
// or the TTL expires. The TTL keeps unclaimed tokens from lingering in memory.

// kiroSsoChoiceTTL bounds how long exchanged tokens wait for a profile choice.
const kiroSsoChoiceTTL = 5 * time.Minute

// pendingKiroSsoChoice parks one exchanged credential awaiting a profile pick.
type pendingKiroSsoChoice struct {
	result    *auth.KiroSsoResult
	machineId string
	profiles  []KiroProfile
	// expiresAt is the ACCESS TOKEN's absolute expiry, stamped at exchange time.
	// It must not be recomputed from ExpiresIn at finalize time: the operator can
	// sit on the picker for minutes, and an expiry overstated by that gap would
	// make the proactive refresh (tokenRefreshSkewSeconds) miss the real deadline.
	expiresAt int64
	// deadline is when this stash self-destructs. A re-stash (invalid pick keeps
	// the entry alive for another attempt) reuses the ORIGINAL deadline so
	// repeated invalid picks cannot extend how long tokens sit in memory.
	deadline time.Time
	timer    *time.Timer
}

// stashPendingKiroSsoChoice parks an exchanged credential plus its discovered
// profiles under the SSO session id, self-expiring at pending.deadline.
func stashPendingKiroSsoChoice(sessionID string, pending *pendingKiroSsoChoice) {
	// Identity-checked expiry: only delete the entry if it is still THIS stash.
	// A plain delete-by-key could race a re-stash — Stop() on an already-fired
	// timer is a no-op, and the fired callback would then destroy the fresh entry.
	pending.timer = time.AfterFunc(time.Until(pending.deadline), func() {
		pendingKiroSsoChoicesMu.Lock()
		if cur, ok := pendingKiroSsoChoices[sessionID]; ok && cur == pending {
			delete(pendingKiroSsoChoices, sessionID)
			logger.Debugf("[KiroSSO] Pending profile choice for session %s expired", sessionID)
		}
		pendingKiroSsoChoicesMu.Unlock()
	})
	pendingKiroSsoChoicesMu.Lock()
	// A repeated stash for the same session replaces the previous one; stop the
	// superseded timer (best-effort — the identity check above covers the rest).
	if prev, ok := pendingKiroSsoChoices[sessionID]; ok && prev.timer != nil {
		prev.timer.Stop()
	}
	pendingKiroSsoChoices[sessionID] = pending
	pendingKiroSsoChoicesMu.Unlock()
}

// takePendingKiroSsoChoice removes and returns the parked credential, or nil.
func takePendingKiroSsoChoice(sessionID string) *pendingKiroSsoChoice {
	pendingKiroSsoChoicesMu.Lock()
	defer pendingKiroSsoChoicesMu.Unlock()
	pending, ok := pendingKiroSsoChoices[sessionID]
	if !ok {
		return nil
	}
	delete(pendingKiroSsoChoices, sessionID)
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return pending
}

// dropPendingKiroSsoChoice discards a parked credential (cancel / TTL expiry).
func dropPendingKiroSsoChoice(sessionID string) {
	if pending := takePendingKiroSsoChoice(sessionID); pending != nil {
		logger.Debugf("[KiroSSO] Dropped pending profile choice for session %s", sessionID)
	}
}

// apiSelectKiroSsoProfile finishes a deferred hosted-portal sign-in: the operator
// picked one of the discovered profiles, so pin it and create the account.
func (h *Handler) apiSelectKiroSsoProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"sessionId"`
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.SessionID == "" || req.ProfileArn == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and profileArn are required"})
		return
	}

	pending := takePendingKiroSsoChoice(req.SessionID)
	if pending == nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profile choice expired or already completed; sign in again"})
		return
	}

	// Only an ARN that was actually offered may be pinned — reject anything else
	// and re-park the stash so the operator can pick again.
	valid := false
	for _, p := range pending.profiles {
		if p.Arn == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		// Re-park with the ORIGINAL deadline: an invalid pick must not reset the TTL.
		stashPendingKiroSsoChoice(req.SessionID, pending)
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}

	pending.result.ProfileArn = req.ProfileArn
	h.finalizeKiroSsoAccount(w, pending.result, pending.machineId, pending.expiresAt)
}

// apiListAccountKiroProfiles GET /accounts/{id}/kiro-profiles
// Runs the multi-region profile discovery for an EXISTING account so the
// operator can see every Kiro profile the credential can reach (e.g. a US and
// an EU profile) and re-pin via POST. external_idp only: other auth methods
// carry an authoritative region already.
func (h *Handler) apiListAccountKiroProfiles(w http.ResponseWriter, r *http.Request, id string) {
	account, status, errMsg := h.lookupAccountForProfileOps(id)
	if account == nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	profiles, err := DiscoverKiroProfiles(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"profiles": profiles,
		"current":  strings.TrimSpace(account.ProfileArn),
	})
}

// apiSwitchAccountKiroProfile POST /accounts/{id}/kiro-profiles {profileArn}
// Re-pins an existing external_idp account to another discovered profile. The
// requested ARN is validated against a fresh discovery (stateless: no stash to
// expire) before overwriting the cached ProfileArn; the pool reload makes the
// data-plane region switch take effect on the next request.
func (h *Handler) apiSwitchAccountKiroProfile(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.ProfileArn == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is required"})
		return
	}

	account, status, errMsg := h.lookupAccountForProfileOps(id)
	if account == nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	profiles, err := DiscoverKiroProfiles(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	valid := false
	for _, p := range profiles {
		if p.Arn == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}

	if err := config.UpdateAccountProfileArn(id, req.ProfileArn); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()

	// The model list is region-scoped: a profile in another region can expose a
	// different set, so refresh the cache for the new pin right away instead of
	// serving the old region's models until the next scheduled refresh. Failure
	// here must not undo the switch — the ARN is already persisted — so it is
	// reported alongside success rather than as an error status.
	account.ProfileArn = req.ProfileArn
	modelsRefreshed := true
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		modelsRefreshed = false
		logger.Warnf("[ProfileArn] Model refresh after profile switch failed for %s: %v", accountEmailForLog(account), err)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"profileArn":      req.ProfileArn,
		"modelsRefreshed": modelsRefreshed,
	})
}

// lookupAccountForProfileOps fetches an account copy (with the pool's freshest
// tokens) for profile discovery/switch, restricted to external_idp accounts.
// Returns (nil, httpStatus, message) when not found (404) or not eligible (400).
func (h *Handler) lookupAccountForProfileOps(id string) (*config.Account, int, string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		return nil, 404, "Account not found"
	}
	if !strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
		return nil, 400, "profile switching is only supported for external_idp accounts"
	}
	// 与 apiRefreshAccountModels 一致：用 pool 中运行时最新 token 探测，避免用到
	// 已被刷新淘汰的磁盘态 token。
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	return account, 200, ""
}

// apiPollKiroSso reports the hosted-portal sign-in status. While the user is signing in it
// returns completed=false; once the listener captures the authorization code it exchanges it,
// persists the account (AuthMethod "external_idp" for an Azure tenant, "social" otherwise), and
// returns completed=true. The profileArn is resolved lazily on first use (the EXTERNAL_IDP
// token type header is now sent on CodeWhisperer calls), so it is not required here.
func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId is required"})
		return
	}
	h.kiroSsoLifecycleMu.Lock()
	defer h.kiroSsoLifecycleMu.Unlock()
	// The auth session is consumed before an exchanged credential is parked.
	// Repeated polls must therefore consult the pending-choice store while holding
	// the lifecycle lock before attempting another auth poll.
	if profiles, warnings, expiresAt, ok := h.pendingKiroSsoProfileChoice(req.SessionID); ok {
		writeKiroSsoProfileChoice(w, profiles, warnings, expiresAt)
		return
	}

	result, status, err := pollKiroSsoAuthForAdmin(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    "pending",
		})
		return
	}

	// Authorization completed. processCompletedKiroSsoResult owns the whole
	// post-exchange flow for this route: it probes every candidate region for
	// Kiro profiles and then either creates the account directly (0 or 1
	// profile found — 0 keeps the historical lazy-resolution behaviour) or parks
	// the exchanged credential in kiroSsoProfileChoiceStore and offers the
	// operator a choice (2+ profiles). It also carries the partial-region
	// warnings, so a region that failed to answer is never presented as an
	// authoritative empty result.
	if err := validateCompletedKiroSsoResult(result, status); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	account, profiles, warnings, expiresAt, err := h.processCompletedKiroSsoResult(req.SessionID, *result)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(profiles) >= 2 {
		writeKiroSsoProfileChoice(w, profiles, warnings, expiresAt)
		return
	}
	writeKiroSsoCompleted(w, account)
}

// buildKiroSsoAccount assembles the persisted account record from an exchanged
// hosted-portal credential. Shared by the immediate path (0/1 profile) and the
// deferred path (operator picked one of several profiles), so both create
// byte-identical accounts. expiresAt is the absolute token expiry stamped at
// exchange time — never derived from ExpiresIn here, because on the deferred
// path minutes may have passed since the exchange.
func buildKiroSsoAccount(result *auth.KiroSsoResult, machineId string, expiresAt int64) config.Account {
	return config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    result.AuthMethod,
		Provider:      result.Provider,
		Region:        result.Region,
		ProfileArn:    result.ProfileArn,
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     machineId,
	}
}

// finalizeKiroSsoAccount persists the account and writes the completed response.
// Used by the deferred path (/auth/kiro-sso/select-profile), where the operator
// has already picked one of the offered profiles, so there is no choice left to
// present here.
func (h *Handler) finalizeKiroSsoAccount(w http.ResponseWriter, result *auth.KiroSsoResult, machineId string, expiresAt int64) {
	account := buildKiroSsoAccount(result, machineId, expiresAt)

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()

	// Resolve profile ARN + fetch account info in background.
	// external_idp accounts: resolve profileArn via ListAvailableProfiles (with
	// TokenType: EXTERNAL_IDP header) first — this is the correct health-check.
	// GetUsageLimits is optional metadata; failure does NOT ban the account.
	safeGo(func() {
		// Step 1: Resolve profile ARN (required for runtime generate endpoint)
		if _, err := ResolveProfileArn(&account); err != nil {
			logger.Warnf("[apiPollKiroSso] Profile ARN resolve failed for external_idp account %s: %v", account.Email, err)
		}
		// Step 2: Usage/subscription info (best-effort, non-fatal)
		if _, err := RefreshAccountInfo(&account); err != nil {
			logger.Warnf("[apiPollKiroSso] Account info refresh failed for external_idp account %s: %v", account.Email, err)
		}
		h.fetchAndCacheAccountModels(&account)
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "completed",
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}
func (h *Handler) writeAddAccountError(w http.ResponseWriter, err error, rotatedRefreshToken ...string) {
	if errors.Is(err, config.ErrDuplicateAccountID) ||
		errors.Is(err, config.ErrDuplicateRefreshToken) ||
		errors.Is(err, config.ErrDuplicateAPIKey) {
		w.WriteHeader(http.StatusConflict)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	payload := map[string]string{"error": err.Error()}
	if len(rotatedRefreshToken) > 0 {
		if rotated := strings.TrimSpace(rotatedRefreshToken[0]); rotated != "" {
			// Microsoft may have already invalidated the original refresh token.
			// Surface the rotated value so operators can retry import without a
			// full interactive re-login.
			payload["rotatedRefreshToken"] = rotated
			payload["hint"] = "The identity provider rotated the refresh token before persistence failed; retry import with rotatedRefreshToken"
		}
	}
	json.NewEncoder(w).Encode(payload)
}
