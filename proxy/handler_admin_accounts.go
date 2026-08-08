package proxy

import (
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":         a.ID,
			"email":      a.Email,
			"userId":     a.UserId,
			"nickname":   a.Nickname,
			"authMethod": a.AuthMethod,
			"provider":   a.Provider,
			"region":     a.Region,
			"enabled":    a.Enabled,
			"banStatus":  a.BanStatus,
			"banReason":  a.BanReason,
			"banTime":    a.BanTime,
			"expiresAt":  a.ExpiresAt,
			// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream exposed only
			// "hasToken" via accountBearerToken(&a) != "". a.HasUpstreamCredential()
			// is the same predicate over both credential kinds, and the fork's three
			// extra fields are consumed by the admin UI, so the fork's superset wins.
			"hasToken":          a.HasUpstreamCredential(),
			"credentialKind":    a.CredentialKind(),
			"kiroApiKeyMask":    maskKiroIssuedAPIKey(a.KiroApiKey),
			"profilePinned":     a.ProfilePinned,
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"modelAllowList":    a.ModelAllowList,
			"regionOverride":    a.RegionOverride,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
		}
	}
	json.NewEncoder(w).Encode(result)
}
func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}

	// Kiro API-key accounts: the key IS the credential. Validate it, normalize the
	// auth method, and mirror it into AccessToken so pool routing / model refresh
	// (which gate on a non-empty AccessToken) treat the account as ready. ExpiresAt
	// stays 0 so the token-refresh paths skip it — API keys are never refreshed.
	// Detection is case-insensitive (matching IsApiKeyCredential) and also triggers
	// when a bare kiroApiKey is supplied without an explicit authMethod.
	account.KiroApiKey = strings.TrimSpace(account.KiroApiKey)
	if account.IsApiKeyCredential() || account.KiroApiKey != "" {
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		// Reject a contradictory payload: a key plus a different, explicit OAuth
		// method would otherwise be silently rewritten to api_key.
		if am := strings.TrimSpace(account.AuthMethod); am != "" && !account.IsApiKeyCredential() {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey cannot be combined with authMethod " + am})
			return
		}
		account.AuthMethod = "api_key"
		account.ExpiresAt = 0
		account.AccessToken = account.KiroApiKey
		// Discover the region the key actually serves. The panel deliberately omits
		// region for api_key adds, so without this an EU-provisioned key would inherit
		// the us-east-1 default below and 403 on every upstream call, permanently.
		// A transient upstream failure must not be reported as a bad key, so it maps
		// to 502 (retry) rather than 400 (caller error).
		region, info, retryable, err := resolveApiKeyRegion(account.KiroApiKey, account.Region)
		if err != nil {
			status := 400
			if retryable {
				status = 502
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		account.Region = region
		// The probe already paid for the identity round-trip; keep it so the account
		// shows a real email in the panel and can be deduplicated by UserId later.
		if info != nil {
			if account.Email == "" {
				account.Email = info.Email
			}
			if account.UserId == "" {
				account.UserId = info.UserId
			}
		}
	}

	if account.Region == "" {
		account.Region = "us-east-1"
	}
	// NOTE: this endpoint previously hard-rejected every api_key account with
	// "Kiro API-key onboarding is not enabled on this endpoint", deferring to a
	// future phase that would "validate the key and region first". That
	// validation is the resolveApiKeyRegion probe above — it verifies the key
	// against upstream, resolves the real data-plane region (rather than
	// defaulting to us-east-1 and 403ing forever), and backfills identity. With
	// it in place the block was rejecting the very requests it was waiting for,
	// so it is gone. The dedicated probe/commit endpoints
	// (/auth/kiro-api-key/probe + /commit, kiro_apikey_admin.go) still exist for
	// the two-step operator flow; this is the direct single-call path.
	// Locked in by TestApiAddAccountApiKeyBranch and the region tests beside it.

	// Validate/normalize the data-plane region override supplied at creation.
	if account.RegionOverride != "" {
		normalized, ok := validateRegionOverride(account.RegionOverride)
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid regionOverride: must be an AWS region like us-east-1, or empty"})
			return
		}
		account.RegionOverride = normalized
	}

	// Handle API-key credential creation
	if account.KiroApiKey != "" || strings.EqualFold(account.AuthMethod, "api_key") || strings.EqualFold(account.AuthMethod, "apikey") {
		// Reject empty API key
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		// Normalize authMethod and set expiry
		account.AuthMethod = "api_key"
		account.ExpiresAt = 0
		account.AccessToken = account.KiroApiKey // Set accessToken to kiroApiKey for pool compatibility
		// Don't call RefreshToken for API-key accounts
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token（或是 API-key 账号），立即拉取并缓存模型列表
	if account.Enabled && (account.AccessToken != "" || account.IsApiKeyCredential()) {
		acc := account
		safeGo(func() {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}
func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// parseModelAllowList coerces a decoded JSON value into a clean, de-duplicated
// list of model IDs for Account.ModelAllowList. Accepts a JSON array of strings;
// null / non-array / all-empty yields nil (= no restriction). Order is preserved.
func parseModelAllowList(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	seen := make(map[string]bool, len(arr))
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validateRegionOverride normalizes and loosely validates a data-plane region
// override string. Empty (after trim) is valid and means "clear the override".
// A non-empty value must look like an AWS region label: lowercase DNS-safe
// segments (letters/digits) joined by hyphens, at least two segments, ending in
// a digit group — permissive enough for us-gov-east-1 / multi-part partitions and
// future regions, strict enough to keep it safe for hostname construction
// (it is concatenated into q.<region>.amazonaws.com). Returns (normalized, ok).
func validateRegionOverride(v string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return "", true // clears the override
	}
	if len(s) > 40 {
		return "", false
	}
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return "", false
	}
	segments := strings.Split(s, "-")
	if len(segments) < 3 {
		return "", false
	}
	for _, seg := range segments {
		if seg == "" {
			return "", false
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
				return "", false
			}
		}
	}
	// The last segment must be a digit group (the region index, e.g. "1").
	last := segments[len(segments)-1]
	for _, r := range last {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return s, true
}
func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Region override is applied atomically AFTER the whole-struct write below,
	// so it cannot be lost to a concurrent RefreshAccountInfo. Validate it up
	// front (typed): a present value must be a JSON string, and must pass the
	// AWS-region shape check. Reject wrong types / malformed values with 400
	// rather than silently ignoring them.
	regionOverrideProvided := false
	regionOverrideValue := ""
	if raw, present := updates["regionOverride"]; present {
		regionOverrideProvided = true
		switch tv := raw.(type) {
		case string:
			normalized, ok := validateRegionOverride(tv)
			if !ok {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "Invalid regionOverride: must be an AWS region like us-east-1, or empty to clear"})
				return
			}
			regionOverrideValue = normalized
		case nil:
			regionOverrideValue = "" // explicit null clears the override
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid regionOverride: must be a string or null"})
			return
		}
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}
	if v, ok := updates["modelAllowList"]; ok {
		// Accept an array of model IDs (empty array / null clears the restriction).
		existing.ModelAllowList = parseModelAllowList(v)
	}

	// Editable Bedrock fields — only for bedrock accounts. Region and credentials
	// change which upstream/model set the account resolves, so clear the cached model
	// discovery when any of them move. (Region is deliberately NOT editable for Kiro
	// accounts here: their Region drives OIDC endpoints and must not be changed by a
	// generic field update.)
	if existing.IsBedrock() {
		bedrockChanged := false
		if v, ok := updates["region"].(string); ok && strings.TrimSpace(v) != existing.Region {
			existing.Region = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockApiKey"].(string); ok {
			existing.BedrockAPIKey = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockAccessKeyId"].(string); ok {
			existing.BedrockAccessKeyID = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockSecretAccessKey"].(string); ok {
			existing.BedrockSecretAccessKey = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockUseConverse"].(bool); ok {
			existing.BedrockUseConverse = v
		}
		// bedrockRegions is a JSON array of extra candidate regions; accept string
		// items and drop blanks.
		if v, ok := updates["bedrockRegions"].([]interface{}); ok {
			var regions []string
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					regions = append(regions, strings.TrimSpace(s))
				}
			}
			existing.BedrockRegions = regions
			bedrockChanged = true
		}
		// Guard the same either/or invariant the add endpoint enforces: an update must
		// not leave the account with no usable credential (it would then fail every
		// request pre-stream and be perpetually excluded).
		if existing.BedrockAPIKey == "" && (existing.BedrockAccessKeyID == "" || existing.BedrockSecretAccessKey == "") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "bedrock account needs either an API key or an access key + secret"})
			return
		}
		if bedrockChanged {
			clearBedrockModelCache(existing.ID)
			clearBedrockRegionRoutes(existing.ID)
		}
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Apply the region override AFTER the whole-struct write, via the atomic
	// config-layer patch, so it (and its ProfileArn reset) cannot be lost to a
	// concurrent RefreshAccountInfo whole-struct write. When it actually changed,
	// clear the profile-resolution cooldown and the stale model cache so the next
	// request re-probes and re-lists models in the NEW region.
	regionOverrideChanged := false
	if regionOverrideProvided {
		changed, err := config.UpdateAccountRegionOverride(id, regionOverrideValue)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		regionOverrideChanged = changed
		existing.RegionOverride = regionOverrideValue
		if changed {
			existing.ProfileArn = ""
			existing.ProfilePinned = false
			clearProfileArnResolutionCooldown(existing)
			h.pool.ClearModelList(id)
		} else if regionOverrideValue != "" && existing.ProfileArn != "" &&
			!arnRegionAllowed(existing, existing.ProfileArn) {
			// Same override value, but the cached ARN is in a different region
			// (e.g. left over from before the override, or repoisoned). Repair it:
			// blank the ARN so the next call re-resolves in the pinned region.
			existing.ProfileArn = ""
			_ = config.UpdateAccountProfileArn(id, "")
			clearProfileArnResolutionCooldown(existing)
			h.pool.ClearModelList(id)
			regionOverrideChanged = true // trigger the re-fetch below
		}
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表；或区域覆盖变更后重新拉取。
	//
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's CONDITION is kept and
	// upstream's safeGo body is kept. Both parts matter:
	//   - regionOverrideChanged is the reason this branch re-fetches at all after a
	//     region pin moves; without it the account keeps serving the stale model
	//     list from the OLD region (and the assignments above become dead code).
	//   - HasUpstreamCredential() replaces AccessToken != "", which only recognised
	//     OAuth accounts — an api_key / Bedrock account never re-listed its models.
	//   - safeGo (not a bare `go func`) contains a panic in the refresh instead of
	//     taking the whole process down.
	if existing.Enabled && existing.HasUpstreamCredential() && ((!oldEnabled) || regionOverrideChanged) {
		acc := *existing
		safeGo(func() {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for account %s: %v", acc.Email, err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.HasUpstreamCredential() {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			a := acc
			safeGo(func() {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// Refresh OAuth credentials only. Kiro API keys, custom_api and bedrock
			// accounts skip straight to the metadata refresh below.
			//
			// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. Upstream routed this
			// batch action through refreshAccountToken (serialized, persist-then-publish,
			// and it now reports failures instead of silently skipping) — adopted. The
			// fork contributes the credential-kind guard: CanRefreshUpstreamCredential
			// excludes api_key/custom_api/bedrock accounts, which upstream's bare
			// `RefreshToken != ""` check would have attempted to OAuth-refresh.
			if account.CanRefreshUpstreamCredential() {
				if _, err := h.refreshAccountToken(account, true); err != nil {
					logger.Warnf("[BatchRefresh] Token refresh failed for %s: %v", account.Email, err)
					failCount++
					continue
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts have no Kiro credential to exercise; test them by sending a
	// real minimal chat request THROUGH the linked upstream pool and returning its reply.
	if account.IsBedrock() {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		// An explicit test must reflect LIVE state, not a cached verdict: drop the
		// learned region routes so the test re-sweeps every candidate region. Bedrock
		// per-region access can flap, so a stale "not callable anywhere" negative-cache
		// entry would otherwise mask a region whose access just reopened.
		clearBedrockRegionRoutes(account.ID)
		reply, err := h.bedrockTestReply(account, tReq.Model)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}
	if account.IsCustomApi() {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		reply, err := customApiTestReply(account.BaseURL, account.KiroApiKey, tReq.Model)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		h.handleAccountFailure(account, err)
		status := statusForUpstreamError(err)
		applyRetryAfterHeader(w, err)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts have no Kiro token/usage to refresh. "Refresh" for them means
	// re-validating the upstream key against its /api/me quota and reloading the model
	// list from the upstream /v1/models — never a Kiro/AWS call.
	if account.IsBedrock() {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Bedrock account; nothing to refresh"})
		return
	}
	if account.IsCustomApi() {
		quota, err := probeCustomApiQuota(account.BaseURL, account.KiroApiKey)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		info := quota.toAccountInfo(time.Now().Unix())
		if updateErr := config.UpdateAccountInfo(id, info); updateErr != nil {
			logger.Warnf("[Refresh] custom_api quota persist failed for %s: %v", account.ID, updateErr)
		}
		if err := h.fetchAndCacheAccountModels(account); err != nil {
			logger.Warnf("[Refresh] custom_api model reload failed for %s: %v", account.ID, err)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "info": info})
		return
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if !account.CanRefreshUpstreamCredential() {
			return nil
		}
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's single serialized
		// refresh entry point replaces this inline copy. refreshAccountToken now
		// persists BEFORE publishing to the pool and routes the returned profile ARN
		// through acceptRefreshedProfileArn, so the fork's pin/region-override
		// enforcement is preserved while the lost-update race is closed.
		_, err := h.refreshAccountToken(account, true)
		return err
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if shouldRetryAccountRefreshOnError(errMsg) {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":           account.ID,
		"email":        account.Email,
		"userId":       account.UserId,
		"nickname":     account.Nickname,
		"accessToken":  account.AccessToken,
		"refreshToken": account.RefreshToken,
		"clientId":     account.ClientID,
		"clientSecret": account.ClientSecret,
		"kiroApiKey":   account.KiroApiKey,
		"authMethod":   account.AuthMethod,
		"provider":     account.Provider,
		"region":       account.Region,
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union of both sides' export
		// fields. profileArn appears on both; the duplicate was dropped.
		"regionOverride": account.RegionOverride,
		"profileArn":     account.ProfileArn,
		"profilePinned":  account.ProfilePinned,
		"tokenEndpoint":  account.TokenEndpoint,
		"issuerUrl":      account.IssuerURL,
		"scopes":         account.Scopes,
		// Upstream-only external_idp fields. The rest of upstream's side of this
		// conflict duplicated keys already emitted above (a duplicate key in a map
		// literal is a compile error, not a silent overwrite), so only these two
		// genuinely new ones are kept.
		"idpClientId":       account.IdPClientID,
		"loginHint":         account.LoginHint,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts serve whatever the linked pool serves: load the model list
	// from the upstream provider's /v1/models, not from Kiro/AWS.
	if account.IsBedrock() {
		// Prefer live-discovered callable models (access agreement AVAILABLE); fall
		// back to the account/default static map when discovery is unavailable (e.g.
		// the key lacks bedrock:ListFoundationModels).
		ids := h.cachedOrDiscoverBedrockModels(account)
		if len(ids) == 0 {
			for _, v := range account.BedrockModelMap {
				ids = append(ids, v)
			}
		}
		if len(ids) == 0 {
			for _, v := range defaultBedrockModelMap {
				ids = append(ids, v)
			}
		}
		// The panel expects {success, models:[{modelId}]} (same shape as custom_api),
		// not a bare string array. Annotate each model with its learned callable
		// region (from lazy routing / prewarm) when known, so the UI can show where a
		// model actually works. "region": "" means not yet probed.
		models := make([]map[string]interface{}, 0, len(ids))
		for _, mid := range ids {
			region := ""
			if r, ok := getBedrockRoute(account.ID, mid); ok && r.callable {
				region = r.region
			}
			models = append(models, map[string]interface{}{"modelId": mid, "region": region})
		}
		h.pool.SetModelList(account.ID, ids)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "models": models, "candidateRegions": candidateRegions(account)})
		return
	}
	if account.IsCustomApi() {
		models, err := probeCustomApiModels(account.BaseURL, account.KiroApiKey)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(id, modelIDs)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "models": models})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存。The routing cache + aggregate list respect the
	// per-account allow-list, but the response returns ALL native models so the
	// admin UI can present the full set for the operator to choose from.
	accountModels, modelIDs := filterModelsByAllowList(account, models)
	h.pool.SetModelList(id, modelIDs)
	// See refreshModelsCache: limits come from the unfiltered upstream list.
	recordModelTokenLimits(models)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, accountModels)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"models":         models,
		"modelAllowList": account.ModelAllowList,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fork added
		// KiroAPIKey (so an api_key account survives a backup/restore round-trip);
		// upstream added TokenEndpoint/IssuerURL/Scopes (so an external_idp account
		// can be refreshed after restore). Dropping either side would silently
		// export a credential that cannot be re-imported.
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		// Present for api_key accounts so backup/restore round-trips.
		KiroAPIKey string `json:"kiroApiKey,omitempty"`
		Region     string `json:"region,omitempty"`
		ExpiresAt  int64  `json:"expiresAt"`
		AuthMethod string `json:"authMethod,omitempty"`
		Provider   string `json:"provider,omitempty"`
		// External IdP (enterprise SSO) refresh material.
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerURL     string `json:"issuerUrl,omitempty"`
		Scopes        string `json:"scopes,omitempty"`
		// IdPClientID / LoginHint complete the external_idp set: the IdP client id
		// is a distinct credential from ClientID, and login_hint is what the IdP
		// needs to route a re-auth to the right identity. Both were emitted by
		// upstream's side of the literal below but were missing from this struct,
		// so a restored external_idp account lost them.
		IdPClientID string `json:"idpClientId,omitempty"`
		LoginHint   string `json:"loginHint,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		ProfileArn   string             `json:"profileArn,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	// NOTE: RegionOverride is intentionally NOT included in this export. This is
	// a CLIProxyAPI-interop credential schema, not our internal config; the
	// override is a proxy-local operational setting (like ProxyURL, which is also
	// omitted here). Operators re-apply the data-plane region pin via the admin UI
	// after an import. Documented as a known export limitation.
	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream added an early
		// api_key branch here that appended a flat ExportCredentials and `continue`d.
		// It was removed because it short-circuited the shared path below, which is a
		// superset for key accounts: the shared path emits the key in the dedicated
		// KiroAPIKey field (upstream put it in AccessToken, which the fork's importer
		// does not read back as a key), resolves the real data-plane region via
		// accountDataPlaneRegionForResponse instead of the raw a.Region, and carries
		// the subscription/usage/profileArn fields. Pinned by
		// TestExportAccountsIncludesKiroKeyAndEffectiveRegionOnlyInSensitiveExport.

		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else if a.AuthMethod == auth.MicrosoftSSOAuthMethod {
				idp = auth.MicrosoftSSOProvider
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}
		// api_key accounts have no OAuth material; keep the lowercase token so the
		// importer round-trips it back into the same normalization path.
		if a.IsApiKeyCredential() {
			authMethod = "api_key"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportRegion := a.Region
		if a.IsKiroAPIKeyCredential() {
			exportRegion = accountDataPlaneRegionForResponse(a)
		}
		exportAccounts = append(exportAccounts, ExportAccount{
			ID:         a.ID,
			Email:      a.Email,
			Nickname:   a.Nickname,
			Idp:        idp,
			UserId:     a.UserId,
			ProfileArn: a.ProfileArn,
			MachineId:  a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				KiroAPIKey:   a.KiroApiKey,
				// exportRegion resolves an api_key account's real data-plane
				// region rather than the raw stored one, so a restored backup
				// does not come back pinned to the wrong region. (The fork's
				// behaviour is kept over upstream's plain a.Region.)
				Region:     exportRegion,
				ExpiresAt:  a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod: authMethod,
				Provider:   a.Provider,
				// External IdP refresh material (upstream).
				TokenEndpoint: a.TokenEndpoint,
				IssuerURL:     a.IssuerURL,
				Scopes:        a.Scopes,
				// Upstream's side of this conflict re-listed Region / ExpiresAt /
				// AuthMethod / Provider / IssuerURL / Scopes, which are already set
				// above (a duplicate field in a struct literal is a compile error).
				// Its Region: a.Region is deliberately NOT taken: exportRegion above
				// resolves an api_key account's real data-plane region, so taking the
				// raw stored value would restore the account pinned to the wrong one.
				IdPClientID: a.IdPClientID,
				LoginHint:   a.LoginHint,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}
func (h *Handler) apiGetAccountDiagnostics(w http.ResponseWriter, r *http.Request) {
	diagnostics := h.pool.Diagnostics()
	summary := map[string]int{
		"total":          len(diagnostics),
		"available":      0,
		"disabled":       0,
		"cooldown":       0,
		"tokenExpiring":  0,
		"quotaExhausted": 0,
		"notInPool":      0,
	}
	for _, d := range diagnostics {
		if d.Available {
			summary["available"]++
		}
		switch d.Reason {
		case "disabled":
			summary["disabled"]++
		case "cooldown":
			summary["cooldown"]++
		case "token_expiring":
			summary["tokenExpiring"]++
		case "quota_exhausted":
			summary["quotaExhausted"]++
		case "not_in_pool":
			summary["notInPool"]++
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"summary":     summary,
		"diagnostics": diagnostics,
	})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}
