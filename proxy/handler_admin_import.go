package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
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
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// Same cross-region profile + identity repair as the interactive IdC login:
		// the pasted SSO token's region is the portal region, not necessarily the
		// region the CodeWhisperer profile lives in.
		hydrateAccountAfterLogin(&account)

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}
func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's decodeImportRequest is
	// kept. Upstream's side of this conflict was an inline anonymous struct decoded
	// straight from the body, which accepts camelCase only; decodeImportRequest
	// accepts BOTH that camelCase shape and the helper's native snake_case
	// (CLIProxyAPI_*.json), so it is a strict superset of what upstream parsed.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	// decodeImportRequest accepts both the helper's native snake_case
	// (CLIProxyAPI_*.json) and the camelCase the existing UI/API send, so a raw
	// helper document and the legacy payload both work through one path.
	req, err := decodeImportRequest(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream kept ~300 lines of
	// per-credential-kind import logic inline in this handler. The fork had already
	// converged every import path onto h.importOne, so the inline copy is dropped
	// in favour of the shared core -- otherwise this endpoint and the four other
	// import entry points would drift. Upstream's three genuine additions were
	// PORTED INTO importOne rather than discarded:
	//   1. RefreshTokenFingerprint is recorded from the PRE-refresh token, so a
	//      credential cannot be re-imported after the provider rotates it;
	//   2. for external_idp, a client-supplied profileArn must appear in the set
	//      DiscoverKiroProfiles returns for the refreshed token, so an arbitrary
	//      data-plane ARN cannot be pinned onto a working credential;
	//   3. duplicate-import failures answer through writeAddAccountError.
	// importOne is the single source of truth for every credential-import path
	// (this endpoint, /auth/import-cli-json, /auth/import-ide-cache, the batch
	// apply, and the directory watcher), so the persisted account is identical
	// no matter how it arrived. It also owns the api_key branch internally
	// (importKiroAPIKeyCredential: region probe + identity backfill) and the
	// external_idp endpoint allow-list validation, so the inline per-kind
	// handling that used to live here would be a second, drifting copy.
	account, err := h.importOne(req)
	if err != nil {
		// A persistence failure may follow a refresh that already rotated the
		// caller's token; writeAddAccountError surfaces the rotated value (and
		// classifies duplicates as 409) so the operator can retry instead of
		// being left holding a dead credential.
		var persistErr *importPersistError
		if errors.As(err, &persistErr) {
			h.writeAddAccountError(w, persistErr.err, persistErr.rotatedRefreshToken)
			return
		}
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiScanLocalCache handles GET /admin/api/auth/local-cache/scan.
//
// It scans the local AWS SSO cache (~/.aws/sso/cache) for Kiro IDE credentials
// and returns a non-secret summary of each discovered identity so the UI can
// present them for one-click import. Secrets are never included in the response;
// each entry carries a fingerprint used to reference it in the import call.
//
// Returns available=false (not an error) when no local cache exists, so the UI
// can hide the feature on containerized/remote deployments.
func (h *Handler) apiScanLocalCache(w http.ResponseWriter, r *http.Request) {
	dir, _ := auth.LocalCacheDir()
	creds, err := auth.ScanLocalKiroCredentials()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	type item struct {
		Fingerprint string `json:"fingerprint"`
		SourceFile  string `json:"sourceFile"`
		AuthMethod  string `json:"authMethod"`
		Provider    string `json:"provider"`
		Region      string `json:"region"`
		LoginHint   string `json:"loginHint,omitempty"`
		HasClient   bool   `json:"hasClient"`
		Importable  bool   `json:"importable"`
		Reason      string `json:"reason,omitempty"`
	}
	items := make([]item, 0, len(creds))
	for _, c := range creds {
		it := item{
			Fingerprint: c.Fingerprint,
			SourceFile:  c.SourceFile,
			AuthMethod:  c.AuthMethod,
			Provider:    c.Provider,
			Region:      c.Region,
			LoginHint:   c.LoginHint,
			HasClient:   c.HasClient,
			Importable:  true,
		}
		// IdC tokens need a client registration to refresh; flag if missing.
		if c.AuthMethod == "idc" && !c.HasClient {
			it.Importable = false
			it.Reason = "missing clientId/clientSecret (client registration file not found)"
		}
		items = append(items, it)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"available": len(creds) > 0,
		"cacheDir":  dir,
		"count":     len(items),
		"accounts":  items,
	})
}

// apiImportLocalCache handles POST /admin/api/auth/local-cache/import.
//
// Body: {"fingerprints": ["tok-abcd1234", ...]} — the fingerprints returned by
// the scan endpoint. When omitted or empty, every importable credential found
// in the cache is imported. Each credential is run through the same OAuth import
// path as manual credential import, so region probing and profile resolution
// apply automatically.
func (h *Handler) apiImportLocalCache(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprints []string `json:"fingerprints"`
	}
	// Body is optional; ignore decode errors and treat as "import all".
	_ = json.NewDecoder(r.Body).Decode(&req)

	creds, err := auth.ScanLocalKiroCredentials()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(creds) == 0 {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no local Kiro credentials found"})
		return
	}

	want := make(map[string]bool, len(req.Fingerprints))
	for _, f := range req.Fingerprints {
		want[f] = true
	}

	type result struct {
		Fingerprint string `json:"fingerprint"`
		SourceFile  string `json:"sourceFile"`
		Success     bool   `json:"success"`
		AccountID   string `json:"accountId,omitempty"`
		Email       string `json:"email,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	var results []result
	imported := 0
	for _, c := range creds {
		if len(want) > 0 && !want[c.Fingerprint] {
			continue
		}
		res := result{Fingerprint: c.Fingerprint, SourceFile: c.SourceFile}

		if c.AuthMethod == "idc" && !c.HasClient {
			res.Error = "missing clientId/clientSecret"
			results = append(results, res)
			continue
		}

		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream called its own
		// importOAuthCredential(credentialImportPayload{...}) here. The fork had
		// already converged EVERY credential-import entry point onto importOne, so
		// this route is rewired to it rather than reintroducing a second import
		// core that would drift (importOne owns the pre-refresh fingerprint, the
		// duplicate-id / duplicate-credential pre-checks that avoid burning a
		// rotation of a live refresh token, the external_idp ARN allow-list check
		// and the api_key branch — none of which the upstream helper had).
		//
		// IdPClientID and LoginHint are dropped deliberately: importCredentialRequest
		// carries no such fields. LoginHint is a display label only, and the IdP
		// client id is not an input to auth.RefreshToken on this path (ClientID /
		// ClientSecret are the refresh credentials). Nothing auth-bearing is lost.
		account, importErr := h.importOne(importCredentialRequest{
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			AuthMethod:   c.AuthMethod,
			Provider:     c.Provider,
			Region:       c.Region,
			IssuerURL:    c.IssuerURL,
			Scopes:       c.Scopes,
		})
		if importErr != nil {
			res.Error = importErr.Error()
			results = append(results, res)
			continue
		}
		res.Success = true
		res.AccountID = account.ID
		res.Email = account.Email
		imported++
		results = append(results, res)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  imported > 0,
		"imported": imported,
		"total":    len(results),
		"results":  results,
	})
}

// importValidationError marks an import failure caused by bad/missing input (a
// 400) as opposed to an internal/upstream failure (a 500). importErrorStatus
// maps it to the right HTTP status so every caller is consistent.
type importValidationError struct{ msg string }

func (e *importValidationError) Error() string { return e.msg }

// importPersistError marks a failure that happened at (or immediately before)
// the persistence step, after the identity provider may already have rotated the
// caller's refresh token. It carries the rotated value so the operator can retry
// the import without a full interactive re-login, and it preserves the
// underlying config error so writeAddAccountError can still tell a duplicate
// (409) from a genuine save failure (500).
// PORTED FROM UPSTREAM v1.1.5 (3/3).
type importPersistError struct {
	err                 error
	rotatedRefreshToken string
}

func (e *importPersistError) Error() string { return e.err.Error() }
func (e *importPersistError) Unwrap() error { return e.err }
func importErrorStatus(err error) int {
	// importOne returns *importValidationError directly (never wrapped) for bad
	// input, and a plain error for internal/upstream failures.
	if _, ok := err.(*importValidationError); ok {
		return http.StatusBadRequest
	}
	// A duplicate is the caller's conflict, not a server fault: reuse the same
	// classification writeAddAccountError applies to the interactive add path so
	// every import entry point answers 409 for it.
	if errors.Is(err, config.ErrDuplicateAccountID) ||
		errors.Is(err, config.ErrDuplicateRefreshToken) ||
		errors.Is(err, config.ErrDuplicateAPIKey) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// importOne is the single source of truth for turning a normalized credential
// request into a persisted account. apiImportCredentials, apiImportCliJson, and
// the directory watcher all funnel through here so the stored account is
// identical to what apiPollKiroSso writes for an interactive login.
//
// The refresh-before-import invariant is intentional: a credential is only
// persisted after one successful token refresh, because a locally-cached access
// token carries no trustworthy expiry and guessing a short TTL makes the pool
// skip the account forever (see ensureValidToken / Pick expiry handling).
func (h *Handler) importOne(req importCredentialRequest) (config.Account, error) {
	plan := buildImportPlan(0, req)
	if !plan.Valid {
		if len(plan.Errors) > 0 {
			return config.Account{}, &importValidationError{strings.Join(plan.Errors, "; ")}
		}
		return config.Account{}, &importValidationError{"credential import is not valid"}
	}
	req = plan.Request
	if req.AuthMethod == "api_key" {
		return h.importKiroAPIKeyCredential(req)
	}

	// PORTED FROM UPSTREAM v1.1.5 (1/3): a client-supplied account id that is
	// already persisted is rejected BEFORE any outbound refresh. Minting a fresh
	// id on collision (the previous behaviour) silently turned "restore this
	// account" into "create a second copy", and it burned one rotation of the
	// caller's refresh token to do it -- the provider invalidates the old token
	// on refresh, so the operator's original credential file became useless while
	// the duplicate they did not ask for got the working one.
	if id := strings.TrimSpace(req.ID); id != "" && config.AccountIDExists(id) {
		return config.Account{}, &importPersistError{err: config.ErrDuplicateAccountID}
	}

	// Same reasoning for an already-persisted credential: re-importing it would
	// spend a rotation of the live refresh token before AddAccount rejected the
	// duplicate, which invalidates the token the EXISTING account is using and
	// breaks a working account as a side effect of a no-op import. The check
	// covers both the current token and the pre-refresh fingerprint, so a
	// credential stays recognisable across the rotations it has already been
	// through.
	if rt := strings.TrimSpace(req.RefreshToken); rt != "" && config.AccountCredentialExists(rt) {
		return config.Account{}, &importPersistError{err: config.ErrDuplicateRefreshToken}
	}

	var (
		accessToken     string
		expiresAt       int64
		newProfileArn   string
		newRefreshToken string
		refreshErr      error
	)
	// The pre-refresh refresh token is the credential's stable identity: the
	// provider rotates the token itself on every refresh, so fingerprinting the
	// post-refresh value would let the same credential be re-imported endlessly
	// (each import rotating and then fingerprinting a brand-new value).
	// PORTED FROM UPSTREAM v1.1.5 (2/3).
	originalRefreshFingerprint := config.RefreshTokenFingerprint(strings.TrimSpace(req.RefreshToken))
	if accessToken == "" {
		// Mandatory refresh unless a trustworthy external_idp access token with exp
		// was pasted. Carry external_idp material so auth.RefreshToken succeeds.
		tempAccount := &config.Account{
			RefreshToken:  req.RefreshToken,
			ClientID:      req.ClientID,
			ClientSecret:  req.ClientSecret,
			AuthMethod:    req.AuthMethod,
			Region:        req.Region,
			TokenEndpoint: req.TokenEndpoint,
			IssuerURL:     req.IssuerURL,
			Scopes:        req.Scopes,
		}
		accessToken, newRefreshToken, expiresAt, newProfileArn, refreshErr = auth.RefreshToken(tempAccount)
		if refreshErr != nil {
			return config.Account{}, &importValidationError{"Token refresh failed: " + refreshErr.Error()}
		}
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
	}

	// Email: prefer the request-supplied label; else best-effort from the token.
	email := strings.TrimSpace(req.Email)
	userID := strings.TrimSpace(req.UserID)
	if req.AuthMethod == "external_idp" {
		// The freshly refreshed token is the authoritative identity; the labels in
		// a pasted credential file are frequently stale (the operator exported
		// them before an address change, or hand-edited the JSON). Trusting the
		// request over the token persisted an account whose email/userId did not
		// match the credential it actually holds, which makes pool diagnostics
		// and duplicate detection point at the wrong identity.
		if tokenEmail, tokenUserID := auth.ExternalIdpTokenIdentity(accessToken); tokenEmail != "" || tokenUserID != "" {
			if tokenEmail != "" {
				email = tokenEmail
			}
			if tokenUserID != "" {
				userID = tokenUserID
			}
		}
	}
	if email == "" {
		email, _, _ = auth.GetUserInfo(accessToken)
	}
	if email == "" {
		email = emailFromJWT(accessToken)
	}

	accountID := strings.TrimSpace(req.ID)
	if accountID == "" {
		accountID = auth.GenerateAccountID()
	}
	account := config.Account{
		ID:           accountID,
		Email:        email,
		UserId:       userID,
		Nickname:     req.Nickname,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		// Fingerprint the credential the operator actually supplied, not the
		// rotated value now in RefreshToken (see originalRefreshFingerprint).
		RefreshTokenFingerprint: originalRefreshFingerprint,
		ClientID:                req.ClientID,
		ClientSecret:            req.ClientSecret,
		AuthMethod:              req.AuthMethod,
		Provider:                providerWithDefault(req.AuthMethod, req.Provider),
		Region:                  req.Region,
		TokenEndpoint:           req.TokenEndpoint,
		IssuerURL:               req.IssuerURL,
		Scopes:                  req.Scopes,
		// external_idp refresh returns "" for profileArn by design; fall back to
		// the helper-provided ARN. If both empty, ResolveProfileArn discovers it
		// lazily on first use (incl. the cross-region probe for external_idp).
		ProfileArn: pickProfileArn(newProfileArn, req.ProfileArn),
		ProxyURL:   strings.TrimSpace(req.ProxyURL),
		ExpiresAt:  expiresAt,
		Enabled:    true,
		MachineId:  config.GenerateMachineId(),
	}

	if err := verifyImportedProfileArn(&account, req.ProfileArn, newProfileArn); err != nil {
		return config.Account{}, err
	}

	if err := config.AddAccount(account); err != nil {
		// The provider has already rotated the refresh token at this point, so a
		// bare error would leave the operator holding a dead credential with no
		// way to retry. Carry the rotated value out to the caller.
		return config.Account{}, &importPersistError{err: err, rotatedRefreshToken: newRefreshToken}
	}
	return account, nil
}

// verifyImportedProfileArn rejects a client-supplied profileArn that the
// credential's own identity provider does not offer.
//
// Without this check any caller could pin an arbitrary data-plane ARN onto a
// working credential, and every subsequent request would be attributed to that
// profile. A freshly-resolved ARN (returned by the refresh itself) is already
// authoritative and is not re-checked.
//
// Discovery is advisory, not mandatory: when it fails or returns nothing we
// cannot prove the ARN is invalid, and hard-failing there would reject imports
// on any transient upstream trouble -- ResolveProfileArn re-resolves lazily on
// first use anyway. The check therefore only fires when discovery produced a
// definite non-empty offer set that excludes the requested ARN.
func verifyImportedProfileArn(account *config.Account, requestedArn, resolvedArn string) error {
	requested := strings.TrimSpace(requestedArn)
	if requested == "" || strings.TrimSpace(resolvedArn) != "" {
		return nil
	}
	profiles, err := DiscoverKiroProfiles(account)
	if err != nil || len(profiles) == 0 {
		return nil
	}
	for _, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(profile.Arn), requested) {
			return nil
		}
	}
	return &importValidationError{"profileArn " + requested + " is not offered for this credential"}
}

// importKiroAPIKeyCredential restores an explicitly exported Kiro-issued key
// through the same live-validation and atomic-dedup invariants as interactive
// probe/commit onboarding. It never routes API-key material through OAuth refresh.
func (h *Handler) importKiroAPIKeyCredential(req importCredentialRequest) (config.Account, error) {
	// PORTED FROM UPSTREAM v1.1.5: accept the convenience form "ksk_xxx|region"
	// that the Kiro CLI and the login helpers emit. Without the split the pipe
	// and region were treated as part of the secret, so validation rejected the
	// key (or worse, probed with a malformed one) for a shape operators paste
	// routinely. An embedded region only fills in a region the request did not
	// state explicitly; an explicit req.Region still wins.
	if splitKey, splitRegion, splitErr := config.SplitKiroAPIKeyAndRegion(req.KiroAPIKey); splitErr == nil {
		req.KiroAPIKey = splitKey
		if strings.TrimSpace(req.Region) == "" && splitRegion != "" {
			req.Region = splitRegion
		}
	}
	key, err := validateKiroIssuedAPIKey(req.KiroAPIKey)
	if err != nil {
		return config.Account{}, &importValidationError{err.Error()}
	}
	// Region handling mirrors apiAddAccount: an api_key account NEVER re-probes
	// after creation, so a wrong region is permanent (every upstream call 403s).
	// A record that carries no region must therefore DISCOVER the region its key
	// actually serves rather than inherit a us-east-1 default — resolveApiKeyRegion
	// walks the candidate regions and also returns the identity it fetched on the
	// way. A record that does carry one still gets it validated against the key.
	region, _ := validateRegionOverride(req.Region)
	var info *config.AccountInfo
	if region == "" {
		resolved, probed, retryable, resolveErr := resolveApiKeyRegion(key, "")
		if resolveErr != nil {
			// Transient upstream trouble is not a bad key: surface it as a
			// non-validation error so the caller maps it to 502, not 400.
			if retryable {
				return config.Account{}, fmt.Errorf("Kiro API key region discovery failed: %w", resolveErr)
			}
			return config.Account{}, &importValidationError{
				"Kiro API key validation failed: " + classifyKiroAPIKeyProbeError(resolveErr),
			}
		}
		region, info = resolved, probed
	}
	if info == nil {
		probe := &config.Account{
			AuthMethod: "api_key", Provider: "KiroAPIKey", KiroApiKey: key,
			Region: region, RegionOverride: region, MachineId: config.GenerateMachineId(),
		}
		probeErr := error(nil)
		info, probeErr = probeKiroAPIKeyAccount(probe)
		if probeErr != nil || info == nil {
			if probeErr == nil {
				probeErr = fmt.Errorf("empty upstream probe result")
			}
			return config.Account{}, &importValidationError{
				"Kiro API key validation failed: " + classifyKiroAPIKeyProbeError(probeErr),
			}
		}
	}

	accountID := strings.TrimSpace(req.ID)
	if accountID == "" || config.AccountIDExists(accountID) {
		accountID = auth.GenerateAccountID()
	}
	email := strings.TrimSpace(info.Email)
	if email == "" {
		email = strings.TrimSpace(req.Email)
	}
	account := config.Account{
		ID: accountID, Email: email, UserId: strings.TrimSpace(info.UserId),
		Nickname: strings.TrimSpace(req.Nickname), KiroApiKey: key,
		// AccessToken is deliberately left EMPTY. The generic add path mirrors the
		// key there for legacy pool compatibility, but that duplicates a
		// long-lived secret into a second persisted field. It is unnecessary here:
		// config.HasUpstreamCredential and UpstreamBearerToken both special-case
		// IsKiroAPIKeyCredential and read KiroApiKey, so routing and dispatch work
		// from the single copy. Locked in by
		// TestImportKiroAPIKeyCredentialLiveValidatesAndPersists.
		AuthMethod: "api_key", Provider: "KiroAPIKey", Region: region,
		// PORTED FROM UPSTREAM v1.1.5: the machine id for a key-based client is
		// DERIVED from the key (sha256 of "KiroAPIKey/<key>") rather than random,
		// which is what the real Kiro CLI sends. A random id makes the same key
		// look like a different device on every re-import, so upstream-side
		// device heuristics see churn that never happened.
		RegionOverride: region, MachineId: config.MachineIdFromAPIKey(key), Enabled: true,
		BanStatus: "ACTIVE", ExpiresAt: 0, SubscriptionType: info.SubscriptionType,
		SubscriptionTitle: info.SubscriptionTitle, UsageCurrent: info.UsageCurrent,
		UsageLimit: info.UsageLimit, NextResetDate: info.NextResetDate, LastRefresh: time.Now().Unix(),
	}
	if strings.TrimSpace(req.UserID) != "" && account.UserId == "" {
		account.UserId = strings.TrimSpace(req.UserID)
	}
	if account.UsageLimit > 0 {
		account.UsagePercent = account.UsageCurrent / account.UsageLimit
	}
	existing, added, err := config.AddKiroAPIKeyAccountIfAbsent(account)
	if err != nil {
		return config.Account{}, err
	}
	if !added {
		// A key that is already onboarded is a CONFLICT, not malformed input.
		// The interactive probe/commit route (apiCommitKiroAPIKey) already
		// answers 409 for exactly this condition; answering 400 here made the
		// same duplicate look like a bad key depending on which route the
		// operator used, and a 400 tells an automated caller to stop retrying
		// a credential that is in fact fine. The sentinel is wrapped so
		// importErrorStatus/writeAddAccountError classify it while the
		// operator-facing detail (which account already holds it) survives.
		return config.Account{}, &importPersistError{
			err: fmt.Errorf("Kiro API-key account already exists for this identity and region (account %s): %w",
				existing.ID, config.ErrDuplicateAPIKey),
		}
	}
	return account, nil
}

// pickProfileArn prefers a freshly-resolved ARN, falling back to the one the
// helper persisted, then empty (resolved lazily on first use).
func pickProfileArn(resolved, fromHelper string) string {
	if strings.TrimSpace(resolved) != "" {
		return resolved
	}
	return strings.TrimSpace(fromHelper)
}

type importDerivedInfo struct {
	TokenEndpoint bool   `json:"tokenEndpoint"`
	IssuerURL     bool   `json:"issuerUrl"`
	Scopes        bool   `json:"scopes"`
	Source        string `json:"source,omitempty"`
}
type importValidationInfo struct {
	EndpointAllowed bool   `json:"endpointAllowed"`
	EndpointReason  string `json:"endpointReason,omitempty"`
	IssuerAllowed   bool   `json:"issuerAllowed"`
	IssuerReason    string `json:"issuerReason,omitempty"`
}
type importConflictInfo struct {
	Type          string `json:"type"`
	ExistingID    string `json:"existingId,omitempty"`
	ExistingEmail string `json:"existingEmail,omitempty"`
	Severity      string `json:"severity"`
	Message       string `json:"message"`
}
type importPreviewItem struct {
	Index                int                     `json:"index"`
	Valid                bool                    `json:"valid"`
	AuthMethodRaw        string                  `json:"authMethodRaw,omitempty"`
	AuthMethod           string                  `json:"authMethod"`
	AuthMethodNormalized string                  `json:"authMethodNormalized"`
	Provider             string                  `json:"provider"`
	Email                string                  `json:"email,omitempty"`
	Nickname             string                  `json:"nickname,omitempty"`
	Region               string                  `json:"region,omitempty"`
	TokenEndpoint        string                  `json:"tokenEndpoint,omitempty"`
	IssuerURL            string                  `json:"issuerUrl,omitempty"`
	ScopesPreview        string                  `json:"scopesPreview,omitempty"`
	ProfileArn           string                  `json:"profileArn,omitempty"`
	HasRefreshToken      bool                    `json:"hasRefreshToken"`
	HasAccessToken       bool                    `json:"hasAccessToken"`
	HasClientID          bool                    `json:"hasClientId"`
	HasClientSecret      bool                    `json:"hasClientSecret"`
	HasKiroAPIKey        bool                    `json:"hasKiroApiKey"`
	Derived              importDerivedInfo       `json:"derived"`
	Validation           importValidationInfo    `json:"validation"`
	ImportMode           string                  `json:"importMode"`
	TrustOnImport        bool                    `json:"trustOnImport"`
	JWTExpiresAt         int64                   `json:"jwtExpiresAt,omitempty"`
	WillReplaceEmail     bool                    `json:"willReplaceEmail"`
	WillReuseID          bool                    `json:"willReuseId"`
	DuplicateID          bool                    `json:"duplicateId"`
	Conflicts            []importConflictInfo    `json:"conflicts,omitempty"`
	Warnings             []string                `json:"warnings,omitempty"`
	Errors               []string                `json:"errors,omitempty"`
	Request              importCredentialRequest `json:"-"`
}

func buildImportPlan(index int, req importCredentialRequest) importPreviewItem {
	plan := importPreviewItem{Index: index, AuthMethodRaw: strings.TrimSpace(req.AuthMethod), Valid: true, Request: req}
	if plan.Index == 0 {
		plan.Index = 1
	}
	// Region defaulting is OAuth-only. An api_key account never re-probes after
	// creation, so stamping us-east-1 on a region-less key is unrecoverable (every
	// upstream call 403s forever); leaving it empty lets
	// importKiroAPIKeyCredential discover the region the key actually serves.
	if strings.TrimSpace(req.Region) == "" && normalizeAuthMethod(req.AuthMethod, req.TokenEndpoint, req.ClientID, req.ClientSecret) != "api_key" {
		req.Region = "us-east-1"
	}
	req.AuthMethod = normalizeAuthMethod(req.AuthMethod, req.TokenEndpoint, req.ClientID, req.ClientSecret)
	if req.AuthMethod == "api_key" {
		if _, err := validateKiroIssuedAPIKey(req.KiroAPIKey); err != nil {
			plan.Errors = append(plan.Errors, err.Error())
		}
		if strings.TrimSpace(req.AccessToken) != "" || strings.TrimSpace(req.RefreshToken) != "" ||
			strings.TrimSpace(req.ClientID) != "" || strings.TrimSpace(req.ClientSecret) != "" ||
			strings.TrimSpace(req.TokenEndpoint) != "" {
			plan.Errors = append(plan.Errors, "api_key import must not include OAuth credential material")
		}
		// An EMPTY region is valid here and means "discover it": the import then
		// probes the candidate regions and pins whichever one the key serves. A
		// region that IS supplied must still be well-formed, because it narrows
		// the probe to that single region.
		if strings.TrimSpace(req.Region) != "" {
			if region, ok := validateRegionOverride(req.Region); !ok || region == "" {
				plan.Errors = append(plan.Errors, "api_key import region must be a valid AWS region like us-east-1, or empty to discover it")
			} else {
				req.Region = region
			}
		}
	} else if req.KiroAPIKey != "" {
		plan.Errors = append(plan.Errors, "kiroApiKey requires authMethod=api_key")
	}
	derivedTE, derivedIss, derivedScopes := auth.DeriveExternalIdpEndpoints(req.UserID, req.ClientID, req.AccessToken)
	if derivedTE != "" && auth.ValidateExternalIdpEndpoint(derivedTE) == nil &&
		req.AuthMethod != "external_idp" && req.AuthMethod != "api_key" {
		// A bare credential blob (refresh token + client id + access JWT, no
		// tokenEndpoint and no clientSecret) is classified "social" by
		// normalizeAuthMethod, which has already stamped the *social* provider
		// label ("Google") onto the request. Promoting the auth method here
		// without re-deriving that label would persist an external_idp account
		// tagged as Google, so every provider-keyed branch (refresh routing,
		// UI grouping, audit logs) would treat a Microsoft tenant account as a
		// social one. Only labels that are indistinguishable from the previous
		// method's auto-default are re-derived; an explicitly supplied provider
		// is left untouched.
		if strings.TrimSpace(req.Provider) == providerWithDefault(req.AuthMethod, "") {
			req.Provider = providerWithDefault("external_idp", "")
		}
		req.AuthMethod = "external_idp"
	}
	if req.AuthMethod == "external_idp" {
		if strings.TrimSpace(req.TokenEndpoint) == "" && derivedTE != "" {
			req.TokenEndpoint = derivedTE
			plan.Derived.TokenEndpoint = true
		}
		if strings.TrimSpace(req.IssuerURL) == "" && derivedIss != "" {
			req.IssuerURL = derivedIss
			plan.Derived.IssuerURL = true
		}
		if strings.TrimSpace(req.Scopes) == "" && derivedScopes != "" {
			req.Scopes = derivedScopes
			plan.Derived.Scopes = true
		}
		if plan.Derived.TokenEndpoint || plan.Derived.IssuerURL || plan.Derived.Scopes {
			if strings.TrimSpace(req.UserID) != "" {
				plan.Derived.Source = "userId"
			} else {
				plan.Derived.Source = "accessTokenIssuer"
			}
		}
		if strings.TrimSpace(req.TokenEndpoint) == "" || strings.TrimSpace(req.ClientID) == "" {
			plan.Errors = append(plan.Errors, "external_idp import requires token_endpoint and client_id (or userId/accessToken to derive them)")
		}
		if strings.TrimSpace(req.TokenEndpoint) != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.TokenEndpoint); err != nil {
				plan.Validation.EndpointReason = err.Error()
				plan.Errors = append(plan.Errors, "external IdP endpoint rejected: "+err.Error())
			} else {
				plan.Validation.EndpointAllowed = true
				plan.Validation.EndpointReason = "host allow-listed"
			}
		}
		if strings.TrimSpace(req.IssuerURL) != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.IssuerURL); err != nil {
				plan.Validation.IssuerReason = err.Error()
				plan.Errors = append(plan.Errors, "external IdP issuer rejected: "+err.Error())
			} else {
				plan.Validation.IssuerAllowed = true
				plan.Validation.IssuerReason = "host allow-listed"
			}
		}
	}
	if req.AuthMethod != "api_key" && strings.TrimSpace(req.RefreshToken) == "" {
		plan.Errors = append(plan.Errors, "refreshToken is required")
	}
	plan.JWTExpiresAt = auth.ExpFromAccessTokenJWT(req.AccessToken)
	plan.TrustOnImport = req.AuthMethod == "external_idp" && strings.TrimSpace(req.AccessToken) != "" && plan.JWTExpiresAt > 0
	plan.ImportMode = "live_refresh"
	if req.AuthMethod == "api_key" {
		plan.ImportMode = "live_api_key_probe"
	}
	if plan.TrustOnImport {
		plan.ImportMode = "trust_access_token_exp"
		plan.Warnings = append(plan.Warnings, "access token JWT exp can be used without a live refresh, but it does not prove the token is accepted upstream")
	}
	if len(plan.Errors) > 0 {
		plan.Valid = false
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		email = emailFromJWT(req.AccessToken)
	}
	plan.AuthMethod = req.AuthMethod
	plan.AuthMethodNormalized = req.AuthMethod
	plan.Provider = providerWithDefault(req.AuthMethod, req.Provider)
	plan.Email = email
	plan.Nickname = strings.TrimSpace(req.Nickname)
	plan.Region = req.Region
	plan.TokenEndpoint = req.TokenEndpoint
	plan.IssuerURL = req.IssuerURL
	plan.ScopesPreview = req.Scopes
	plan.ProfileArn = req.ProfileArn
	plan.HasRefreshToken = strings.TrimSpace(req.RefreshToken) != ""
	plan.HasAccessToken = strings.TrimSpace(req.AccessToken) != ""
	plan.HasClientID = strings.TrimSpace(req.ClientID) != ""
	plan.HasClientSecret = strings.TrimSpace(req.ClientSecret) != ""
	plan.HasKiroAPIKey = strings.TrimSpace(req.KiroAPIKey) != ""
	for _, acc := range config.GetAccounts() {
		if email != "" && strings.EqualFold(strings.TrimSpace(acc.Email), email) {
			plan.WillReplaceEmail = true
			plan.Conflicts = append(plan.Conflicts, importConflictInfo{Type: "same_email", ExistingID: acc.ID, ExistingEmail: acc.Email, Severity: "warning", Message: "an existing account uses the same email"})
		}
		if strings.TrimSpace(req.ID) != "" && acc.ID == strings.TrimSpace(req.ID) {
			plan.DuplicateID = true
			plan.Conflicts = append(plan.Conflicts, importConflictInfo{Type: "same_id", ExistingID: acc.ID, ExistingEmail: acc.Email, Severity: "warning", Message: "an existing account uses the same ID; import will generate a new ID"})
		}
	}
	plan.WillReuseID = strings.TrimSpace(req.ID) != "" && !plan.DuplicateID
	plan.Request = req
	return plan
}
func previewImportRequests(reqs []importCredentialRequest) []importPreviewItem {
	items := make([]importPreviewItem, 0, len(reqs))
	for i, req := range reqs {
		items = append(items, buildImportPlan(i+1, req))
	}
	return items
}
func (h *Handler) apiPreviewCredentials(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	req, err := decodeImportRequest(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	items := previewImportRequests([]importCredentialRequest{req})
	h.appendAuditLog(AuditLog{Category: "import", Action: "preview_credentials", Status: "success", Source: "credentials", SafeDetails: map[string]string{"count": strconv.Itoa(len(items))}})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(items), "items": items})
}

type importApplyDecision struct {
	Action            string `json:"action"`
	ExistingAccountID string `json:"existingAccountId"`
}
type importApplyRequest struct {
	Raw       json.RawMessage                `json:"raw"`
	Decisions map[string]importApplyDecision `json:"decisions"`
}

func (h *Handler) apiApplyCredentials(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	payload := body
	var applyReq importApplyRequest
	if err := json.Unmarshal(body, &applyReq); err == nil && len(applyReq.Raw) > 0 {
		payload = applyReq.Raw
	}
	reqs, warnings, err := normalizeCliJson(payload)
	if err != nil {
		single, singleErr := decodeImportRequest(payload)
		if singleErr != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error(), "warnings": warnings})
			return
		}
		reqs = []importCredentialRequest{single}
	}
	var imported []map[string]interface{}
	var skipped []int
	var errs []string
	for i, req := range reqs {
		decision := importApplyDecision{Action: "create_new"}
		if applyReq.Decisions != nil {
			if d, ok := applyReq.Decisions[strconv.Itoa(i+1)]; ok {
				decision = d
			}
		}
		switch decision.Action {
		case "skip":
			skipped = append(skipped, i+1)
			continue
		case "", "create_new":
			account, impErr := h.importOne(req)
			if impErr != nil {
				errs = append(errs, fmt.Sprintf("item %d: %s", i+1, impErr.Error()))
				continue
			}
			imported = append(imported, map[string]interface{}{"id": account.ID, "email": account.Email, "authMethod": account.AuthMethod, "action": "create_new"})
			h.appendAuditLog(AuditLog{Category: "import", Action: "import_credentials", Status: "success", AccountID: account.ID, AccountEmail: account.Email, AuthMethod: account.AuthMethod, Provider: account.Provider, Source: "apply", SafeDetails: map[string]string{"decision": "create_new"}})
		case "replace_existing":
			if strings.TrimSpace(decision.ExistingAccountID) == "" {
				errs = append(errs, fmt.Sprintf("item %d: existingAccountId is required for replace_existing", i+1))
				continue
			}
			account, impErr := h.importOne(req)
			if impErr != nil {
				errs = append(errs, fmt.Sprintf("item %d: %s", i+1, impErr.Error()))
				continue
			}
			newID := account.ID
			account.ID = decision.ExistingAccountID
			if err := config.ReplaceAccountAndDelete(decision.ExistingAccountID, newID, account); err != nil {
				errs = append(errs, fmt.Sprintf("item %d: replace failed: %s", i+1, err.Error()))
				continue
			}
			imported = append(imported, map[string]interface{}{"id": account.ID, "email": account.Email, "authMethod": account.AuthMethod, "action": "replace_existing"})
			h.appendAuditLog(AuditLog{Category: "import", Action: "replace_account", Status: "success", AccountID: account.ID, AccountEmail: account.Email, AuthMethod: account.AuthMethod, Provider: account.Provider, Source: "apply", SafeDetails: map[string]string{"replacedId": decision.ExistingAccountID}})
		default:
			errs = append(errs, fmt.Sprintf("item %d: unsupported action %q", i+1, decision.Action))
		}
	}
	if len(imported) > 0 {
		h.pool.Reload()
	}
	if len(imported) == 0 && len(skipped) == 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": strings.Join(errs, "; "), "warnings": warnings})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "imported": imported, "skipped": skipped, "errors": errs, "warnings": warnings})
}
func (h *Handler) apiPreviewCliJson(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	reqs, warnings, err := normalizeCliJson(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error(), "warnings": warnings})
		return
	}
	items := previewImportRequests(reqs)
	h.appendAuditLog(AuditLog{Category: "import", Action: "preview_credentials", Status: "success", Source: "cli_json", SafeDetails: map[string]string{"count": strconv.Itoa(len(items))}})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(reqs), "items": items, "warnings": warnings})
}

type ideCacheImportOptions struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	DirectProxy   *bool  `json:"directProxy,omitempty"`
	ForceProvider string `json:"forceProvider,omitempty"`
}

func applyIdeCacheImportOptions(req importCredentialRequest, opts ideCacheImportOptions) importCredentialRequest {
	if strings.EqualFold(strings.TrimSpace(opts.Mode), "enterprise_m365") {
		// Kiro IDE has emitted two Enterprise cache shapes in the wild:
		// external_idp with tokenEndpoint+clientId, and IdC with clientIdHash plus a
		// sibling client registration. Only force AzureAD/external_idp when the cache
		// actually has external-IdP refresh material; otherwise keep the valid IdC
		// credential instead of turning it into an unrefreshable external_idp account.
		if strings.TrimSpace(req.TokenEndpoint) != "" && strings.TrimSpace(req.ClientID) != "" {
			req.AuthMethod = "external_idp"
			if strings.TrimSpace(opts.ForceProvider) != "" {
				req.Provider = strings.TrimSpace(opts.ForceProvider)
			} else {
				req.Provider = "AzureAD"
			}
		} else if req.AuthMethod == "idc" && strings.TrimSpace(req.Provider) == "" {
			req.Provider = "Enterprise"
		}
	}
	if opts.DirectProxy == nil || *opts.DirectProxy {
		req.ProxyURL = directProxyOptOut
	}
	return req
}
func (h *Handler) apiPreviewIdeCache(w http.ResponseWriter, r *http.Request) {
	var body ideCacheImportOptions
	_ = json.NewDecoder(r.Body).Decode(&body)
	path := ideCachePath(body.Path)
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	req = applyIdeCacheImportOptions(req, body)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "source": path, "count": 1, "items": previewImportRequests([]importCredentialRequest{req})})
}

// apiImportCliJson imports one or more raw CLIProxyAPI_*.json helper documents. It
// accepts the helper's native snake_case external_idp shape and funnels it into the
// importOne core the legacy endpoint uses. Per-item results are returned so a
// partial batch still reports which credentials landed.
func (h *Handler) apiImportCliJson(w http.ResponseWriter, r *http.Request) {
	// Bounded at 1 MiB to match apiPreviewCliJson, the preview half of this same
	// pair, which already wrapped its body. The import half did not — the only
	// asymmetry among the four credential import/preview endpoints.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}

	reqs, warnings, err := normalizeCliJson(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"error":    err.Error(),
			"warnings": warnings,
		})
		return
	}

	var imported []map[string]interface{}
	var errs []string
	for i, req := range reqs {
		account, impErr := h.importOne(req)
		if impErr != nil {
			errs = append(errs, fmt.Sprintf("item %d: %s", i+1, impErr.Error()))
			continue
		}
		imported = append(imported, map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
		})
	}

	if len(imported) > 0 {
		h.pool.Reload()
	}

	// Match the batch convention in apiImportSsoToken: 500 only when nothing landed.
	if len(imported) == 0 {
		w.WriteHeader(500)
		errMsg := "no credentials imported"
		if len(errs) > 0 {
			errMsg = strings.Join(errs, "; ")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"error":    errMsg,
			"warnings": warnings,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"imported": imported,
		"errors":   errs,
		"warnings": warnings,
	})
}

// apiImportIdeCache imports the credential the Kiro IDE already cached on this
// host (~/.aws/sso/cache/kiro-auth-token.json), with no browser sign-in. The
// optional JSON body { "path": "..." } overrides the cache location (else the
// KIRO_IDE_CACHE env var, else the default path). It funnels through the same
// importOne core as every other import path, so the persisted account is
// identical to an interactive Enterprise SSO login.
func (h *Handler) apiImportIdeCache(w http.ResponseWriter, r *http.Request) {
	var body ideCacheImportOptions
	// Body is optional; ignore a decode error (including an empty body).
	_ = json.NewDecoder(r.Body).Decode(&body)

	path := ideCachePath(body.Path)
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	req = applyIdeCacheImportOptions(req, body)

	account, err := h.importOne(req)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	logger.Infof("[Import] %s (account %s)", describeIdeCacheImport(path, account.AuthMethod), account.Email)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  path,
		"account": map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
			"provider":   account.Provider,
			"profileArn": account.ProfileArn,
			"proxyURL":   account.ProxyURL,
		},
	})
}
