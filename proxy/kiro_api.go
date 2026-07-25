package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	kiroRestAPIBase               = "https://codewhisperer.us-east-1.amazonaws.com"
	profileArnUnsupportedCooldown = 24 * time.Hour
)

var profileArnResolutionCooldowns sync.Map

func regionFromProfileArn(profileArn string) string {
	parts := strings.SplitN(strings.TrimSpace(profileArn), ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "codewhisperer" {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// kiroRegion returns the AWS data-plane region for Kiro / Q calls.
// Prefer profileArn because account.Region is the auth/OIDC region and can
// differ from the profile's region.
func kiroRegion(account *config.Account) string {
	return kiroRegionForProfile(account, "")
}

func kiroRegionForProfile(account *config.Account, profileArn string) string {
	// A manual data-plane region override is a HARD pin: it wins over every
	// ARN-derived or auth region so all data-plane calls target it.
	if account != nil {
		if ov := account.EffectiveRegionOverride(); ov != "" {
			return ov
		}
	}
	if r := regionFromProfileArn(profileArn); r != "" {
		return r
	}
	if account != nil {
		if r := regionFromProfileArn(account.ProfileArn); r != "" {
			return r
		}
		if r := strings.TrimSpace(account.Region); r != "" {
			return r
		}
	}
	return "us-east-1"
}

// arnRegionAllowed reports whether a profile ARN may be cached/used for this
// account given its region override. With no override, any ARN is allowed
// (today's behavior). With an override set, the ARN's embedded region MUST equal
// the override — otherwise accepting it would produce a host/ARN region mismatch
// (the exact failure the override exists to prevent). An empty ARN is allowed
// (it means "not yet resolved"); callers gate dispatch on emptiness separately.
func arnRegionAllowed(account *config.Account, arn string) bool {
	if account == nil {
		return true
	}
	ov := account.EffectiveRegionOverride()
	if ov == "" {
		return true
	}
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return true
	}
	return regionFromProfileArn(arn) == ov
}

// acceptRefreshedProfileArn writes an auth-refresh-returned profile ARN back to
// the account (in memory + persisted) UNLESS a region override is set and the
// ARN's region does not match it — in which case the write is skipped so a
// background/manual refresh cannot silently repoison the pin with an ARN from
// another region. Returns true when the ARN was accepted and written. A blank
// ARN is a no-op (returns false). This is the single chokepoint every refresh
// caller must route through instead of writing account.ProfileArn directly.
func acceptRefreshedProfileArn(account *config.Account, arn string) bool {
	if account == nil {
		return false
	}
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return false
	}
	if account.ProfilePinned {
		// A refresh may echo the already-selected ARN, but it must never move a
		// manually pinned account to another profile.
		return arn == strings.TrimSpace(account.ProfileArn)
	}
	if !arnRegionAllowed(account, arn) {
		logger.Warnf("[ProfileArn] Refresh returned ARN outside override region %q for %s; not caching (fail closed)",
			account.EffectiveRegionOverride(), account.Email)
		return false
	}
	// Throwaway probe accounts (profile discovery, api_key region probes, and the
	// pre-persist login paths) are never in the config store, so there is nothing
	// to update and UpdateAccountProfileArn would report "account not found".
	// That is not a resolution failure: the ARN was resolved, and the caller only
	// needs it on the in-memory account. Persist only when the account is real,
	// and keep a genuine persist error for a real account fatal so a silently
	// uncached ARN cannot make every later request re-probe.
	if strings.TrimSpace(account.ID) != "" && config.AccountIDExists(account.ID) {
		if updateErr := config.UpdateAccountProfileArn(account.ID, arn); updateErr != nil {
			logger.Warnf("[ProfileArn] Failed to cache refreshed profile ARN for %s: %v", account.Email, updateErr)
			return false
		}
	}
	account.ProfileArn = arn
	return true
}

// regionalizeURL points a hardcoded us-east-1 Kiro endpoint at the profile's
// data-plane region (see regionalizeURLForProfile). It is a no-op for us-east-1.
func regionalizeURL(rawURL string, account *config.Account) string {
	return regionalizeURLForProfile(rawURL, account, "")
}

// regionalizeURLForProfile points a hardcoded us-east-1 Kiro endpoint at the
// data-plane region derived from the profile (payload ARN first, then the account's
// cached ARN, then account.Region). account.Region is the auth/OIDC region and can
// differ from the profile's region, so the profile ARN is preferred.
func regionalizeURLForProfile(rawURL string, account *config.Account, profileArn string) string {
	return regionalizeURLForRegion(rawURL, kiroRegionForProfile(account, profileArn))
}

// regionalizeURLForRegion rewrites a hardcoded us-east-1 Kiro endpoint to target
// the given region. Amazon Q is regional (q.{region}.amazonaws.com), but the
// CodeWhisperer REST host only exists in us-east-1 — every other region is served
// by the regional Amazon Q host instead. So for a non-us-east-1 region BOTH
// us-east-1 hosts (q.us-east-1.* and codewhisperer.us-east-1.*) collapse onto
// q.{region}.amazonaws.com; there is deliberately no codewhisperer.{region} host.
// It is a no-op for us-east-1 or an empty region. This region-targeted primitive
// also backs cross-region profile probing (listAvailableProfilesInRegion).
func regionalizeURLForRegion(rawURL, region string) string {
	region = strings.TrimSpace(region)
	if region == "" || region == "us-east-1" {
		return rawURL
	}
	regionalHost := "q." + region + ".amazonaws.com"
	return strings.NewReplacer(
		"q.us-east-1.amazonaws.com", regionalHost,
		"codewhisperer.us-east-1.amazonaws.com", regionalHost,
	).Replace(rawURL)
}

// defaultKiroProfileRegions is the ordered set of regions probed when an account's
// home region is unknown. us-east-1 is the historical default every login falls
// back to; eu-central-1 is where EU-provisioned Azure-tenant profiles
// (e.g. KiroProfile-eu-central-1) live. Override or extend with the
// KIRO_PROFILE_REGIONS env var (comma-separated) to onboard further regions
// without a code change.
var defaultKiroProfileRegions = []string{"us-east-1", "eu-central-1"}

// kiroProfileRegionCandidates returns the ordered, de-duplicated list of regions
// to probe for an account's Kiro profile. The account's currently-configured region
// is always tried first. Cross-region fallbacks are only added when the home region
// is genuinely unknown — an external_idp (Azure-tenant) login, which defaults to
// us-east-1, or an account with no region at all. An idc/social/Builder ID account
// already carries its real region (from the SSO portal / the us-east-1 default), so
// it is probed against that single region exactly as before — no extra upstream calls
// and no chance of its established region being flipped. KIRO_PROFILE_REGIONS, when
// set, replaces the built-in fallback set (the account region is still tried first).
func kiroProfileRegionCandidates(account *config.Account) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(region string) {
		region = strings.TrimSpace(region)
		if region == "" || seen[region] {
			return
		}
		seen[region] = true
		out = append(out, region)
	}

	// A region override is a HARD pin: probe ONLY the override region. The
	// profile the operator wants must live there; discovering an ARN in any
	// other region would defeat the pin, so no account-region / fallback probing.
	if account != nil {
		if ov := account.EffectiveRegionOverride(); ov != "" {
			return []string{ov}
		}
	}

	if account != nil {
		add(account.Region)
	}
	if !shouldProbeFallbackRegions(account) {
		return out
	}
	if env := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS")); env != "" {
		for _, r := range strings.Split(env, ",") {
			add(r)
		}
		return out
	}
	for _, r := range defaultKiroProfileRegions {
		add(r)
	}
	return out
}

// shouldProbeFallbackRegions reports whether an account's home region is unknown
// enough to justify probing fallback regions. Only external_idp accounts (region
// defaulted to us-east-1 at login) and accounts with no region set qualify; every
// other auth method already carries its authoritative region.
func shouldProbeFallbackRegions(account *config.Account) bool {
	if account == nil {
		return true
	}
	if strings.TrimSpace(account.Region) == "" {
		return true
	}
	// external_idp (Azure-tenant) logins default to us-east-1 and carry no reliable
	// home region, so their profile can live in a different region.
	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
		return true
	}
	// IAM Identity Center accounts (authMethod idc) carry the SSO PORTAL region,
	// which is NOT necessarily where the CodeWhisperer profile lives. Observed on a
	// real tenant: portal ssoins-*.us-east-1.portal.amazonaws.com (region us-east-1)
	// while ListAvailableProfiles returns zero profiles in us-east-1 and the only
	// profile is arn:aws:codewhisperer:eu-central-1:...  Restricting the probe to the
	// portal region therefore yields an empty list and the account fails with "no
	// available Kiro profile" even though a usable profile exists elsewhere. This
	// covers both the Kiro IDE "Enterprise" import (provider Enterprise) and the
	// interactive IAM Identity Center login, which records no provider label at all.
	//
	// Builder ID is deliberately excluded: profile listing is unsupported for it
	// across every region, so extra probing would only repeat a known-403 call.
	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "idc") &&
		!strings.EqualFold(strings.TrimSpace(account.Provider), "BuilderId") {
		return true
	}
	return false
}

// GetUsageLimits 获取账户使用量和订阅信息
func GetUsageLimits(account *config.Account) (*UsageLimitsResponse, error) {
	if err := ensureRestProfileArn(account); err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	url := fmt.Sprintf("%s/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true", kiroRestAPIBase)
	url = regionalizeURL(url, account)
	url = withProfileArnQuery(url, account)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	setKiroHeaders(req, account)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result UsageLimitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetUserInfo 获取用户信息
func GetUserInfo(account *config.Account) (*UserInfoResponse, error) {
	url := regionalizeURL(fmt.Sprintf("%s/GetUserInfo", kiroRestAPIBase), account)

	payload := `{"origin":"KIRO_IDE"}`
	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}

	setKiroHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result UserInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListAvailableModels 获取可用模型列表
func ListAvailableModels(account *config.Account) ([]ModelInfo, error) {
	if err := ensureRestProfileArn(account); err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	models, err := listAvailableModelsOnce(account)
	if err == nil {
		return models, nil
	}
	// Self-heal a poisoned cached profile: a not-in-plan profile (e.g. the
	// account's us-east-1 profile) can be cached ahead of the in-plan one, which
	// makes ListAvailableModels 403 with "not authorized to make this call". This
	// is the same class of failure the usage path self-heals; re-resolve to a
	// usable profile (preferring the in-plan one) and retry once before giving up.
	if isProfileOrPlanAuthzError(err.Error()) {
		if _, ok := reresolveProfileArn(account); ok {
			return listAvailableModelsOnce(account)
		}
	}
	return nil, err
}

func listAvailableModelsOnce(account *config.Account) ([]ModelInfo, error) {
	url := fmt.Sprintf("%s/ListAvailableModels?origin=AI_EDITOR&maxResults=50", kiroRestAPIBase)
	url = regionalizeURL(url, account)
	url = withProfileArnQuery(url, account)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	setKiroHeaders(req, account)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Models, nil
}

// ResolveProfileArn returns the account profile ARN, fetching and caching it
// when it is missing. First tries ListAvailableProfiles; if that returns empty,
// falls back to refreshing the token (which returns profileArn in the response).
func ResolveProfileArn(account *config.Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is nil")
	}
	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		return profileArn, nil
	}
	if account.ProfilePinned {
		return "", fmt.Errorf("manually pinned profile ARN is missing")
	}

	// Kiro API-key credentials are key-scoped. They may expose no listable
	// profile and have no OAuth refresh fallback; callers recognize this as a
	// soft skip and continue without profileArn.
	if account.IsKiroAPIKeyCredential() {
		return "", fmt.Errorf("profile ARN resolution skipped: api_key account uses key-bound profile")
	}

	// Kiro API-key (ksk_) accounts are headless: the profile is bound to the key
	// server-side, so ListAvailableProfiles returns nothing and there is no refresh
	// token to fall back on. Skip resolution entirely and let callers proceed WITHOUT
	// a profileArn (getUsageLimits / generateAssistantResponse are key-scoped). This
	// is a soft skip — isProfileArnResolutionSkippedError matches the message — so
	// ensureRestProfileArn and the data-plane continue instead of hard-failing, and
	// it avoids a fruitless multi-region probe on every request.
	if account.IsApiKeyCredential() {
		return "", fmt.Errorf("profile ARN resolution skipped: api_key account uses key-bound profile")
	}

	profileLookupSuppressed := isProfileArnResolutionSuppressed(account)
	var profileUnsupportedErr error
	var profileUnsupported bool

	if !profileLookupSuppressed {
		// Probe ListAvailableProfiles across candidate regions, retrying transient
		// failures. The home region is unknown at login for Azure-tenant
		// (external_idp) accounts (they default to us-east-1), so the probe is what
		// discovers a profile that lives outside the account's configured region. The
		// cached ARN then drives the data-plane region via kiroRegionForProfile — no
		// separate region persistence is needed (and account.Region stays the auth
		// region, which can legitimately differ from the profile's region).
		profileArn, err := resolveProfileArnAcrossRegions(account)
		if err == nil && profileArn != "" {
			if !acceptRefreshedProfileArn(account, profileArn) {
				return "", fmt.Errorf("failed to cache resolved Kiro profile")
			}
			return profileArn, nil
		}
		profileUnsupportedErr = err
		profileUnsupported = isBuilderIDProfileUnsupportedError(account, err)
	}

	// Fallback: refresh OAuth credentials to get profileArn from the auth response.
	// Under a region override this fallback must NOT silently cache an ARN from
	// a different region — that would recreate the host/ARN mismatch the override
	// prevents. Refuse a mismatched ARN and fail closed instead. Kiro API-key
	// credentials never participate in this lifecycle.
	if account.CanRefreshUpstreamCredential() {
		_, _, _, refreshedArn, refreshErr := auth.RefreshToken(account)
		if refreshErr == nil && refreshedArn != "" {
			if !arnRegionAllowed(account, refreshedArn) {
				logger.Warnf("[ProfileArn] Refreshed profile ARN for %s is not in the override region %q; refusing to cache (fail closed)",
					account.Email, account.EffectiveRegionOverride())
				return "", fmt.Errorf("no available Kiro profile in override region %q", account.EffectiveRegionOverride())
			}
			if !acceptRefreshedProfileArn(account, refreshedArn) {
				return "", fmt.Errorf("failed to cache refreshed Kiro profile")
			}
			return refreshedArn, nil
		}
	}
	if profileLookupSuppressed {
		return "", fmt.Errorf("profile ARN resolution skipped: previous Builder ID profile lookup was unsupported")
	}
	if profileUnsupported {
		suppressProfileArnResolution(account)
		logger.Debugf("[ProfileArn] Builder ID profile lookup unsupported for %s: %v", accountEmailForLog(account), profileUnsupportedErr)
		return "", fmt.Errorf("profile ARN unsupported for Builder ID account")
	}

	return "", fmt.Errorf("no available Kiro profile")
}

func isBuilderIDProfileUnsupportedError(account *config.Account, err error) bool {
	if account == nil || err == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(account.Provider), "BuilderId") {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "HTTP 403") && strings.Contains(msg, "AWS Builder ID is not supported for this operation")
}

func profileArnCooldownKey(account *config.Account) string {
	if account == nil {
		return ""
	}
	provider := strings.TrimSpace(account.Provider)
	if id := strings.TrimSpace(account.ID); id != "" {
		return provider + "\x00" + id
	}
	if userID := strings.TrimSpace(account.UserId); userID != "" {
		return provider + "\x00" + userID
	}
	return provider + "\x00" + strings.TrimSpace(account.Email)
}

func suppressProfileArnResolution(account *config.Account) {
	key := profileArnCooldownKey(account)
	if key == "" {
		return
	}
	profileArnResolutionCooldowns.Store(key, time.Now().Add(profileArnUnsupportedCooldown))
}

// clearProfileArnResolutionCooldown removes any active resolution-suppression
// entry for an account. Called when the region override changes so a manual
// switch always forces a fresh cross-region profile probe instead of being
// blocked by a stale Builder-ID-"unsupported" cooldown.
func clearProfileArnResolutionCooldown(account *config.Account) {
	key := profileArnCooldownKey(account)
	if key == "" {
		return
	}
	profileArnResolutionCooldowns.Delete(key)
}

func isProfileArnResolutionSuppressed(account *config.Account) bool {
	key := profileArnCooldownKey(account)
	if key == "" {
		return false
	}
	value, ok := profileArnResolutionCooldowns.Load(key)
	if !ok {
		return false
	}
	until, ok := value.(time.Time)
	if !ok || time.Now().After(until) {
		profileArnResolutionCooldowns.Delete(key)
		return false
	}
	return true
}

// isProfileOrPlanAuthzError reports whether an upstream error string is about the
// PROFILE or SUBSCRIPTION PLAN rather than the credential itself. These 403s mean
// "this profile/plan can't do this" (wrong profile selected, no active plan,
// resource not authorized for the profile) — the token is still valid, so the
// account must NOT be flipped to BANNED. Genuine token-invalid/expired 401/403s
// deliberately fall through to the auth-ban branch instead.
//
// It only fires for 403s (plan/authorization denials); a 401 is always a token
// problem, never a plan problem, so 401 is excluded here on purpose.
func isProfileOrPlanAuthzError(errMsg string) bool {
	if !strings.Contains(errMsg, "403") {
		return false
	}
	// A 403 that also carries explicit token-invalidity wording is a real auth
	// failure, not a plan issue — let it reach the ban branch.
	lower := strings.ToLower(errMsg)
	if strings.Contains(lower, "invalid") || strings.Contains(lower, "expired") ||
		strings.Contains(lower, "token") {
		return false
	}
	for _, marker := range []string{
		"not authorized",
		"not subscribed",
		"no active subscription",
		"subscription",
		"accessdenied",
		"access denied",
		"profile",
		"not entitled",
		"entitlement",
		"resourcenotfound",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isProfileArnResolutionSkippedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profile ARN resolution skipped")
}

func isProfileArnResolutionUnsupportedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profile ARN unsupported for Builder ID account")
}

func isProfileArnResolutionSoftError(err error) bool {
	return isProfileArnResolutionSkippedError(err) || isProfileArnResolutionUnsupportedError(err)
}

func ensureRestProfileArn(account *config.Account) error {
	if account == nil || strings.TrimSpace(account.ProfileArn) != "" {
		return nil
	}
	profileArn, err := ResolveProfileArn(account)
	if err != nil {
		if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Continuing REST request without profile ARN for %s: %v", accountEmailForLog(account), err)
			return nil
		}
		return err
	}
	account.ProfileArn = profileArn
	return nil
}

// resolveProfileArnAcrossRegions probes ListAvailableProfiles against each
// candidate region (the account's configured region first, then the fallbacks) and
// returns the first profile ARN found. This is what lets an account whose profile
// lives outside its configured region — every Azure-tenant (external_idp) login
// defaults to us-east-1 — discover that profile (e.g. in eu-central-1) on first use.
// The returned ARN carries its own region, which kiroRegionForProfile then uses for
// data-plane calls. A correctly-regioned account resolves on the first probe. A
// Builder ID "unsupported" 403 is authoritative across all regions, so it
// short-circuits the probe rather than repeating per region.
func resolveProfileArnAcrossRegions(account *config.Account) (string, error) {
	var lastErr error
	var found []string
	seen := make(map[string]bool)
	for _, region := range kiroProfileRegionCandidates(account) {
		arns, probeErr := listAllAvailableProfilesWithRetryInRegion(account, region)
		if probeErr != nil {
			lastErr = probeErr
			if isBuilderIDProfileUnsupportedError(account, probeErr) {
				return "", probeErr
			}
			continue
		}
		for _, arn := range arns {
			// Under a region override, refuse any discovered ARN whose embedded
			// region does not match the pin — even if upstream returned it while
			// probing the pinned region. This keeps the ARN and the target host
			// in the same region (fail closed).
			if !arnRegionAllowed(account, arn) {
				continue
			}
			if !seen[arn] {
				seen[arn] = true
				found = append(found, arn)
			}
		}
	}
	if len(found) == 0 {
		if lastErr != nil {
			return "", lastErr
		}
		return "", fmt.Errorf("empty profile list")
	}
	if len(found) == 1 {
		return found[0], nil
	}
	// Multiple profiles across regions (e.g. a not-in-plan KiroProfile-us-east-1 plus
	// an in-plan KiroProfile-eu-central-1). Prefer the one that is actually in a
	// subscription plan rather than blindly taking the first region's profile — that
	// "first wins" bug is what selected the not-in-plan profile and made getUsageLimits
	// 403, which was then misclassified as a ban.
	if usable := selectUsableProfile(account, found); usable != "" {
		return usable, nil
	}
	// None verified usable (e.g. every getUsageLimits probe failed transiently) — fall
	// back to the first discovered profile to preserve prior behavior.
	return found[0], nil
}

// selectUsableProfile returns the first profile ARN whose getUsageLimits call
// succeeds — i.e. the profile is actually attached to a subscription plan. It is
// only used to disambiguate when an account owns more than one profile, so the
// common single-profile path never pays for the extra calls.
func selectUsableProfile(account *config.Account, arns []string) string {
	for _, arn := range arns {
		probe := *account
		probe.ProfileArn = arn
		if r := regionFromProfileArn(arn); r != "" {
			probe.Region = r
		}
		if _, err := GetUsageLimits(&probe); err == nil {
			return arn
		}
	}
	return ""
}

// retryUsageWithReresolvedProfile handles the case where a getUsageLimits call
// failed with a cached profile ARN that turns out to be the wrong one (e.g. an
// account owns a not-in-plan us-east-1 profile AND an in-plan eu-central-1
// profile, and the not-in-plan one was cached). It re-runs the cross-region,
// plan-aware profile probe, and if it discovers a DIFFERENT, usable profile it
// persists that ARN and retries getUsageLimits once. Returns (usage, true) only
// when the retry succeeds; otherwise (nil, false) so the caller falls through to
// its normal error/ban classification.
//
// It deliberately does not fire for genuine suspensions — those must still reach
// the ban classifier — so it skips re-resolution when the error is a temporary
// suspension signal.
func retryUsageWithReresolvedProfile(account *config.Account, origErr error) (*UsageLimitsResponse, bool) {
	if account == nil || origErr == nil {
		return nil, false
	}
	if strings.Contains(origErr.Error(), "TEMPORARILY_SUSPENDED") {
		return nil, false
	}
	if _, ok := reresolveProfileArn(account); !ok {
		return nil, false
	}
	usage, err := GetUsageLimits(account)
	if err != nil {
		return nil, false
	}
	return usage, true
}

// reresolveProfileArn forces a fresh, plan-aware cross-region profile probe,
// ignoring the account's currently-cached ARN. When it finds a DIFFERENT usable
// profile it persists the corrected ARN (and its region) on the account and
// returns (newArn, true). Otherwise it returns ("", false) and leaves the account
// untouched. This is the shared primitive both the usage path and the model-list
// path use to recover from a poisoned cached profile (e.g. a not-in-plan
// us-east-1 profile cached ahead of an in-plan eu-central-1 profile).
func reresolveProfileArn(account *config.Account) (string, bool) {
	if account == nil || account.ProfilePinned {
		return "", false
	}
	oldArn := strings.TrimSpace(account.ProfileArn)

	probe := *account
	probe.ProfileArn = ""
	newArn, err := resolveProfileArnAcrossRegions(&probe)
	if err != nil || strings.TrimSpace(newArn) == "" || newArn == oldArn {
		return "", false
	}
	// Under a region override, refuse a corrected ARN from a different region.
	if !arnRegionAllowed(account, newArn) {
		return "", false
	}

	if !acceptRefreshedProfileArn(account, newArn) {
		return "", false
	}
	// Converge the auth region onto the profile's region for non-override
	// accounts (existing self-heal behavior). When an override is set, leave
	// account.Region (the auth/OIDC region) untouched — the override is
	// data-plane only and token refresh keys off account.Region.
	if account.EffectiveRegionOverride() == "" {
		if r := regionFromProfileArn(newArn); r != "" {
			account.Region = r
		}
	}
	logger.Infof("[ProfileArn] Self-healed profile for %s: %s -> %s (previous profile was not usable)",
		account.Email, oldArn, newArn)
	return newArn, true
}

// listAvailableProfilesWithRetryInRegion calls ListAvailableProfiles against a
// specific region, retrying transient failures (network errors, 5xx, 429) with
// short backoff. An empty profile list or 4xx (other than 429) is treated as
// authoritative and not retried — they reflect account state, not upstream flakiness.
func listAvailableProfilesWithRetryInRegion(account *config.Account, region string) (string, error) {
	const maxAttempts = 3
	backoff := 200 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		profileArn, err := listAvailableProfilesInRegion(account, region)
		if err == nil {
			return profileArn, nil
		}
		lastErr = err
		if !isTransientProfileFetchError(err) || attempt == maxAttempts {
			return "", err
		}
		logger.Debugf("[ProfileArn] ListAvailableProfiles transient failure for %s in %s (attempt %d/%d): %v",
			account.Email, region, attempt, maxAttempts, err)
		time.Sleep(backoff)
		backoff *= 2
	}
	return "", lastErr
}

// listAllAvailableProfilesWithRetryInRegion is the multi-profile variant of
// listAvailableProfilesWithRetryInRegion: it returns every profile ARN a region
// exposes (not just the first), retrying transient failures the same way. This is
// what lets resolveProfileArnAcrossRegions disambiguate an account that owns more
// than one profile (e.g. a not-in-plan us-east-1 profile plus an in-plan
// eu-central-1 profile).
func listAllAvailableProfilesWithRetryInRegion(account *config.Account, region string) ([]string, error) {
	const maxAttempts = 3
	backoff := 200 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		profileArns, err := listAllAvailableProfilesInRegion(account, region)
		if err == nil {
			return profileArns, nil
		}
		lastErr = err
		if !isTransientProfileFetchError(err) || attempt == maxAttempts {
			return nil, err
		}
		logger.Debugf("[ProfileArn] ListAvailableProfiles transient failure for %s in %s (attempt %d/%d): %v",
			account.Email, region, attempt, maxAttempts, err)
		time.Sleep(backoff)
		backoff *= 2
	}
	return nil, lastErr
}

// isTransientProfileFetchError reports whether a ListAvailableProfiles error
// is worth retrying. Network errors and upstream 5xx/429 are transient; other
// HTTP errors and an empty profile list are not.
func isTransientProfileFetchError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "empty profile list") {
		return false
	}
	if strings.HasPrefix(msg, "HTTP ") {
		return strings.HasPrefix(msg, "HTTP 5") || strings.HasPrefix(msg, "HTTP 429")
	}
	// Non-HTTP errors are network/transport level — retry.
	return true
}

// listAvailableProfilesInRegion calls ListAvailableProfiles with the request host
// pointed at a specific region (q.{region} for non-us-east-1, the CodeWhisperer
// REST host for us-east-1). Targeting an explicit region — rather than the account's
// stored one — is what makes cross-region detection possible: the same credential is
// probed against each candidate region until one returns a profile. This single-ARN
// wrapper keeps the historical "first profile wins" contract for the lazy resolver;
// callers that need every profile use listProfileArnsInRegion directly.
func listAvailableProfilesInRegion(account *config.Account, region string) (string, error) {
	arns, err := listAllAvailableProfilesInRegion(account, region)
	if err != nil {
		return "", err
	}
	return arns[0], nil
}

// listAllAvailableProfilesInRegion returns every non-empty profile ARN the region
// exposes for the account, and treats an EMPTY list as an error. This is the
// strict variant used by the lazy resolver, where "this region has no profile"
// must fail so the caller probes the next candidate region. Callers doing
// multi-region discovery (where an empty region is a normal outcome) use
// listProfileArnsInRegion directly.
func listAllAvailableProfilesInRegion(account *config.Account, region string) ([]string, error) {
	arns, err := listProfileArnsInRegion(account, region)
	if err != nil {
		return nil, err
	}
	if len(arns) == 0 {
		return nil, fmt.Errorf("empty profile list")
	}
	return arns, nil
}

// listProfileArnsInRegion calls ListAvailableProfiles against a specific region and
// returns EVERY profile ARN it lists (trimmed, empty entries dropped). An empty
// list is returned as ([], nil) — for multi-region discovery a region with no
// profiles is a normal outcome, not an error.
func listProfileArnsInRegion(account *config.Account, region string) ([]string, error) {
	endpoint := regionalizeURLForRegion(fmt.Sprintf("%s/ListAvailableProfiles", kiroRestAPIBase), region)
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(`{"maxResults":10}`))
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Profiles []struct {
			Arn string `json:"arn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	var arns []string
	for _, profile := range result.Profiles {
		if profileArn := strings.TrimSpace(profile.Arn); profileArn != "" {
			arns = append(arns, profileArn)
		}
	}
	// An empty list is NOT an error here: multi-region discovery treats a region
	// with no profiles as a normal outcome. listAllAvailableProfilesInRegion is
	// the strict wrapper that converts empty into an error for the lazy resolver.
	return arns, nil
}

// listProfileArnsWithRetryInRegion is listProfileArnsInRegion plus the same
// transient-failure retry policy as listAvailableProfilesWithRetryInRegion
// (network errors, 5xx, 429 → short backoff; other errors are authoritative).
func listProfileArnsWithRetryInRegion(account *config.Account, region string) ([]string, error) {
	const maxAttempts = 3
	backoff := 200 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		arns, err := listProfileArnsInRegion(account, region)
		if err == nil {
			return arns, nil
		}
		lastErr = err
		if !isTransientProfileFetchError(err) || attempt == maxAttempts {
			return nil, err
		}
		logger.Debugf("[ProfileArn] ListAvailableProfiles transient failure for %s in %s (attempt %d/%d): %v",
			accountEmailForLog(account), region, attempt, maxAttempts, err)
		time.Sleep(backoff)
		backoff *= 2
	}
	return nil, lastErr
}

// KiroProfile is declared in kiro_profiles.go, which carries the richer
// operator-facing shape (Arn, Region plus Usable/Current/Pinned). The values
// built here populate only Arn and Region; the remaining fields are filled by
// the admin discovery layer that presents the choice.

// DiscoverKiroProfiles probes ListAvailableProfiles against EVERY candidate
// region (the account's configured region first, then the fallbacks — see
// kiroProfileRegionCandidates) and returns all profiles found, de-duplicated by
// ARN. Unlike resolveProfileArnAcrossRegions it does NOT stop at the first
// region that yields a profile: an Azure-tenant (external_idp) account can hold
// profiles in several regions (e.g. a US and an EU Kiro profile), and the caller
// needs the full set to let the operator pick one. Per-region failures are
// tolerated so one unreachable region cannot hide another region's profiles; an
// error is returned only when nothing was found AND at least one region failed
// (a Builder ID "unsupported" 403 is authoritative for all regions and aborts
// immediately, matching the lazy resolver).
func DiscoverKiroProfiles(account *config.Account) ([]KiroProfile, error) {
	var out []KiroProfile
	seen := make(map[string]bool)
	var lastErr error
	for _, region := range kiroProfileRegionCandidates(account) {
		arns, err := listProfileArnsWithRetryInRegion(account, region)
		if err != nil {
			if isBuilderIDProfileUnsupportedError(account, err) {
				return nil, err
			}
			// Surface the failed region: silently skipping it would present a
			// partial list as complete and hide exactly the profile (e.g. EU)
			// this discovery exists to find.
			logger.Warnf("[ProfileArn] Profile discovery failed in %s for %s: %v", region, accountEmailForLog(account), err)
			lastErr = err
			continue
		}
		for _, arn := range arns {
			if seen[arn] {
				continue
			}
			seen[arn] = true
			profileRegion := regionFromProfileArn(arn)
			if profileRegion == "" {
				profileRegion = region
			}
			out = append(out, KiroProfile{Arn: arn, Region: profileRegion})
		}
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

func withProfileArnQuery(rawURL string, account *config.Account) string {
	if account == nil {
		return rawURL
	}
	profileArn := strings.TrimSpace(account.ProfileArn)
	if profileArn == "" {
		return rawURL
	}
	return rawURL + "&profileArn=" + neturl.QueryEscape(profileArn)
}

func setKiroHeaders(req *http.Request, account *config.Account) {
	host := ""
	if req.URL != nil {
		host = req.URL.Host
	}
	headerValues := buildRuntimeHeaderValues(account, host)

	req.Header.Set("Accept", "application/json")
	applyKiroBaseHeaders(req, account, headerValues)
}

// classifyAndBanOnUsageError inspects a GetUsageLimits error and disables the
// account when it signals a hard upstream state (suspension or auth failure).
// Classification routes through the shared pool.IsSuspensionError /
// pool.IsAuthFailure helpers (digit-boundary-aware) instead of bare
// strings.Contains, which previously false-banned accounts when "401"/"403"
// appeared inside request IDs or timestamps. Returns the caller-facing error.
func classifyAndBanOnUsageError(account *config.Account, err error) error {
	// Profile ARN resolution may fail transiently (provisioning lag, cross-region
	// probe failure). The request path treats this as soft (account_failover.go);
	// the background refresh path must too, or a good external_idp account is
	// permanently banned on a transient blip.
	if isProfileUnavailableErrorMessage(err.Error()) {
		return fmt.Errorf("GetUsageLimits: %w", err)
	}
	// A 403 about the PROFILE/PLAN (wrong profile pinned, no active subscription,
	// resource not authorized) is NOT a credential failure and must not ban. This
	// guard is what stops a valid account whose in-plan profile simply had not
	// been selected from being permanently disabled; the caller already tried to
	// self-heal by re-resolving the profile before reaching here. It must sit
	// BEFORE the IsAuthFailure branch, because such a message also carries a 403
	// and would otherwise fall straight through to the ban.
	// Locked in by TestRefreshAccountInfoDoesNotBanOnPlan403.
	if isProfileOrPlanAuthzError(err.Error()) {
		logger.Warnf("[RefreshAccountInfo] Profile/plan authorization error for %s (not treated as ban): %v", account.Email, err)
		return fmt.Errorf("GetUsageLimits: %w", err)
	}
	switch {
	case pool.IsSuspensionError(err):
		logger.Warnf("[RefreshAccountInfo] Account %s is suspended: %v", account.Email, err)
		banAccountInline(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
		return fmt.Errorf("Account suspended: %w", err)
	case pool.IsAuthFailure(err):
		logger.Warnf("[RefreshAccountInfo] Authentication error for %s: %v", account.Email, err)
		banAccountInline(account, "BANNED", "Authentication failed - token invalid or expired")
	}
	return fmt.Errorf("GetUsageLimits: %w", err)
}

// banAccountInline disables an account (banStatus + reason) via config. Used by
// background-refresh paths that have no Handler/pool handle. No-op if the
// account is already disabled with the same status/reason.
func banAccountInline(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}
	updated := *account
	if !updated.Enabled && updated.BanStatus == banStatus && updated.BanReason == banReason {
		return
	}
	updated.Enabled = false
	updated.BanStatus = banStatus
	updated.BanReason = banReason
	updated.BanTime = time.Now().Unix()
	if err := config.UpdateAccount(account.ID, updated); err != nil {
		logger.Errorf("[RefreshAccountInfo] Failed to update account ban status: %v", err)
	}
}

// RefreshAccountInfo 刷新账户信息（使用量、订阅等）
func RefreshAccountInfo(account *config.Account) (*config.AccountInfo, error) {
	info := &config.AccountInfo{
		LastRefresh: time.Now().Unix(),
	}

	usage, err := GetUsageLimits(account)
	if err != nil && !account.IsKiroAPIKeyCredential() {
		// Self-heal a stale/wrong cached profile before treating the failure as a
		// ban. An account that owns more than one profile (e.g. a not-in-plan
		// us-east-1 profile plus an in-plan eu-central-1 profile) may have cached the
		// not-in-plan one, which makes getUsageLimits fail. Re-resolve across all
		// profiles (preferring the in-plan one) and retry once before classifying.
		if healedUsage, healed := retryUsageWithReresolvedProfile(account, err); healed {
			usage, err = healedUsage, nil
		}
	}
	if err != nil {
		// API-key accounts cannot self-heal (never token-refreshed), so no upstream
		// error here should mutate/ban them — a transient blip must not brick a valid,
		// paid key. This also protects the add-time probe, which reuses a throwaway
		// api_key account: it must never write config (classifyAndBanOnUsageError
		// would call UpdateAccount with the throwaway's empty ID). Guard BEFORE the
		// classify/ban helper so both the suspension and auth-fail branches are skipped.
		if account.IsApiKeyCredential() || account.IsCustomApi() {
			return nil, fmt.Errorf("GetUsageLimits: %w", err)
		}
		// classifyAndBanOnUsageError owns the suspension / profile-authz / auth-fail
		// classification. It routes through pool.IsSuspensionError and
		// pool.IsAuthFailure (digit-boundary aware) and treats a profile/plan 403 as
		// soft, which is what the inline strings.Contains chain here used to do less
		// precisely — a "403" appearing inside a request ID false-banned the account.
		return nil, classifyAndBanOnUsageError(account, err)
	}

	// 如果成功获取信息，清除封禁状态（如果之前被标记）
	if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
		logger.Infof("[RefreshAccountInfo] Account %s is now active, clearing ban status", account.Email)

		updatedAccount := *account
		updatedAccount.BanStatus = "ACTIVE"
		updatedAccount.BanReason = ""
		updatedAccount.BanTime = 0

		// 保存更新后的账户状态
		if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
			logger.Errorf("[RefreshAccountInfo] Failed to clear account ban status: %v", updateErr)
		}
	}

	// 解析用户信息
	if usage.UserInfo != nil {
		info.Email = usage.UserInfo.Email
		info.UserId = usage.UserInfo.UserId
	}

	// 解析订阅信息
	if usage.SubscriptionInfo != nil {
		// 优先从 SubscriptionTitle 或 SubscriptionName 解析类型
		titleOrName := usage.SubscriptionInfo.SubscriptionTitle
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionName
		}
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionType
		}
		info.SubscriptionType = parseSubscriptionType(titleOrName)
		info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionTitle
		if info.SubscriptionTitle == "" {
			info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionName
		}
		logger.Debugf("[RefreshAccountInfo] Subscription: type=%s, title=%s, name=%s, parsed=%s",
			usage.SubscriptionInfo.SubscriptionType,
			usage.SubscriptionInfo.SubscriptionTitle,
			usage.SubscriptionInfo.SubscriptionName,
			info.SubscriptionType)
	}

	// 解析使用量
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		info.UsageCurrent = breakdown.CurrentUsage
		info.UsageLimit = breakdown.UsageLimit
		if info.UsageLimit > 0 {
			info.UsagePercent = info.UsageCurrent / info.UsageLimit
		}
	}

	// 解析重置日期
	if usage.NextDateReset != "" {
		if ts, err := usage.NextDateReset.Int64(); err == nil && ts > 0 {
			info.NextResetDate = time.Unix(ts, 0).Format("2006-01-02")
		} else if f, err := usage.NextDateReset.Float64(); err == nil && f > 0 {
			info.NextResetDate = time.Unix(int64(f), 0).Format("2006-01-02")
		}
	}

	// 解析试用配额信息
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		if breakdown.FreeTrialInfo != nil {
			info.TrialUsageCurrent = breakdown.FreeTrialInfo.CurrentUsage
			info.TrialUsageLimit = breakdown.FreeTrialInfo.UsageLimit
			if info.TrialUsageLimit > 0 {
				info.TrialUsagePercent = info.TrialUsageCurrent / info.TrialUsageLimit
			}
			info.TrialStatus = breakdown.FreeTrialInfo.FreeTrialStatus

			// 解析试用到期时间
			if breakdown.FreeTrialInfo.FreeTrialExpiry != "" {
				if ts, err := breakdown.FreeTrialInfo.FreeTrialExpiry.Int64(); err == nil && ts > 0 {
					info.TrialExpiresAt = ts
				} else if f, err := breakdown.FreeTrialInfo.FreeTrialExpiry.Float64(); err == nil && f > 0 {
					info.TrialExpiresAt = int64(f)
				}
			}
		}
	}

	return info, nil
}

func parseSubscriptionType(raw string) string {
	upper := strings.ToUpper(raw)
	if strings.Contains(upper, "PRO_PLUS") || strings.Contains(upper, "PROPLUS") {
		return "PRO_PLUS"
	}
	if strings.Contains(upper, "POWER") {
		return "POWER"
	}
	if strings.Contains(upper, "PRO") {
		return "PRO"
	}
	return "FREE"
}

// 响应结构体
type UsageLimitsResponse struct {
	UsageBreakdownList []UsageBreakdown  `json:"usageBreakdownList"`
	NextDateReset      json.Number       `json:"nextDateReset"`
	SubscriptionInfo   *SubscriptionInfo `json:"subscriptionInfo"`
	UserInfo           *UserInfo         `json:"userInfo"`
}

type UsageBreakdown struct {
	ResourceType  string         `json:"resourceType"`
	CurrentUsage  float64        `json:"currentUsage"`
	UsageLimit    float64        `json:"usageLimit"`
	Currency      string         `json:"currency"`
	Unit          string         `json:"unit"`
	OverageRate   float64        `json:"overageRate"`
	FreeTrialInfo *FreeTrialInfo `json:"freeTrialInfo"`
	Bonuses       []BonusInfo    `json:"bonuses"`
}

type FreeTrialInfo struct {
	CurrentUsage    float64     `json:"currentUsage"`
	UsageLimit      float64     `json:"usageLimit"`
	FreeTrialStatus string      `json:"freeTrialStatus"`
	FreeTrialExpiry json.Number `json:"freeTrialExpiry"`
}

type BonusInfo struct {
	BonusCode    string      `json:"bonusCode"`
	DisplayName  string      `json:"displayName"`
	CurrentUsage float64     `json:"currentUsage"`
	UsageLimit   float64     `json:"usageLimit"`
	ExpiresAt    json.Number `json:"expiresAt"`
	Status       string      `json:"status"`
}

type SubscriptionInfo struct {
	SubscriptionName  string `json:"subscriptionName"`
	SubscriptionTitle string `json:"subscriptionTitle"`
	SubscriptionType  string `json:"subscriptionType"`
	Status            string `json:"status"`
	UpgradeCapability string `json:"upgradeCapability"`
}

type UserInfo struct {
	Email  string `json:"email"`
	UserId string `json:"userId"`
}

type UserInfoResponse struct {
	Email  string `json:"email"`
	UserId string `json:"userId"`
	Idp    string `json:"idp"`
	Status string `json:"status"`
}

type ModelInfo struct {
	ModelId        string   `json:"modelId"`
	ModelName      string   `json:"modelName"`
	Description    string   `json:"description"`
	InputTypes     []string `json:"supportedInputTypes"`
	RateMultiplier float64  `json:"rateMultiplier"`
	TokenLimits    *struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	} `json:"tokenLimits"`
}
