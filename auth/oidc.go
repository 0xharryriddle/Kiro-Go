package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"sync"
	"time"
)

// oidcTokenURL 构造 idc/builderId 刷新 endpoint。测试可替换以拦截网络调用。
var oidcTokenURL = func(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
}

// socialTokenURL 构造 social 刷新 endpoint。测试可替换以拦截网络调用。
var socialTokenURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
}

// refreshSkewSeconds mirrors pool.tokenRefreshSkewSeconds: a token within this
// many seconds of expiry is treated as expiring and refreshed proactively.
// Kept local to auth to avoid a pool→auth import cycle.
const refreshSkewSeconds int64 = 120

var (
	// refreshMu guards refreshLocks creation only; each per-account lock is then
	// held for the duration of one refresh.
	refreshMu sync.Mutex
	// refreshLocks maps accountID → that account's refresh mutex. One lock per
	// account serializes only that account's background refreshes, reducing the
	// global contention the handler-level tokenRefreshMu imposed. tokenRefreshMu
	// still guards the request-path refresh (handler.go:2148) and remains part of
	// the documented lock order (account.go:250) — intentionally not removed.
	// Account IDs are stable UUIDs; the map is bounded by the total number of
	// accounts ever seen, which is negligible for this deployment.
	refreshLocks = map[string]*sync.Mutex{}
)

// refreshLockFor returns the per-account refresh mutex, creating it on first use.
func refreshLockFor(id string) *sync.Mutex {
	refreshMu.Lock()
	lock, ok := refreshLocks[id]
	if !ok {
		lock = &sync.Mutex{}
		refreshLocks[id] = lock
	}
	refreshMu.Unlock()
	return lock
}

// refreshTokenDirect resolves the proxy-aware HTTP client and dispatches to the
// provider-specific refresh. It performs no locking or persistence — used both
// for id-less accounts (login validation, which has no shared state to
// coordinate against) and as the inner step of the locked RefreshToken.
func refreshTokenDirect(account *config.Account) (string, string, int64, string, error) {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream added this API-key guard
	// directly in RefreshToken. It is placed here instead so BOTH entry points (the
	// locked RefreshToken and the id-less direct path) reject key credentials — an
	// api_key account has no refresh token, so a refresh attempt is always a bug.
	if config.IsAPIKeyAccount(account) {
		return "", "", 0, "", fmt.Errorf("API Key credentials do not support token refresh")
	}
	// Resolve per-account proxy: account.ProxyURL > global config. A direct opt-out
	// must not fall back to the global proxy; imported IDE accounts may need to
	// mirror the IDE's no-proxy path while other accounts still use global proxy.
	proxyURL := account.ProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	// When require-proxy is on and no proxy is configured, refuse the refresh
	// rather than connecting directly — a direct refresh POST would leak the
	// server's real IP before CallKiroAPI's gate can fire. The error string
	// carries "require-proxy" so the failover classifier cools down + rotates.
	if proxyURL == "" && config.GetRequireProxy() {
		return "", "", 0, "", fmt.Errorf("require-proxy: no proxy configured for account")
	}
	client := GetAuthClientForProxy(proxyURL)

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): two rival external-IdP refresh
	// implementations collided here. Resolution keeps upstream's refresh (the
	// strict superset: it validates the issuer, pins the token endpoint to the
	// issuer's tenant, and normalizes scopes) and upstream's tolerant auth-method
	// comparison, while keeping the fork's switch shape for the other providers.
	// MicrosoftSSOAuthMethod is the same "external_idp" string the fork already
	// stored, so this one branch serves accounts from BOTH onboarding paths.
	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), MicrosoftSSOAuthMethod) {
		// Accounts imported by the fork's older paths can lack issuerUrl/scopes
		// (they were optional before upstream's validation existed). Backfill them
		// from the material we do have rather than failing the refresh: without
		// this, every pre-merge external_idp account would start erroring with
		// "Microsoft issuer must end with /<tenant>/v2.0".
		tokenEndpoint, issuerURL, scopes := externalIdpRefreshInputs(account)
		return refreshExternalIdpToken(
			account.RefreshToken,
			account.ClientID,
			tokenEndpoint,
			issuerURL,
			scopes,
			client,
		)
	}
	switch account.AuthMethod {
	case "social":
		return refreshSocialToken(account.RefreshToken, client)
	default:
		return refreshOIDCToken(account.RefreshToken, account.ClientID, account.ClientSecret, account.Region, client)
	}
}

// externalIdpRefreshInputs returns the token endpoint, issuer, and scopes to use
// for an external-IdP refresh, reconstructing whichever ones the stored account
// omits. Pre-merge accounts were persisted before issuer/scope validation
// existed, so they routinely carry an empty IssuerURL or Scopes; upstream's
// refresh rejects both. Derivation reuses upstream's helpers, which re-validate
// the host allow-list, so a recovered endpoint is no less gated than a stored
// one. Anything that cannot be recovered is passed through unchanged and the
// refresh itself reports the specific validation failure.
func externalIdpRefreshInputs(account *config.Account) (tokenEndpoint, issuerURL, scopes string) {
	tokenEndpoint = strings.TrimSpace(account.TokenEndpoint)
	issuerURL = strings.TrimSpace(account.IssuerURL)
	scopes = strings.TrimSpace(account.Scopes)

	// Issuer missing: recover it from the tenant-specific token endpoint.
	if issuerURL == "" && tokenEndpoint != "" {
		if normalizedEndpoint, derivedIssuer, derivedScopes := ExternalIdpConfigurationFromTokenEndpoint(tokenEndpoint, account.ClientID); derivedIssuer != "" {
			issuerURL = derivedIssuer
			if normalizedEndpoint != "" {
				tokenEndpoint = normalizedEndpoint
			}
			if scopes == "" {
				scopes = derivedScopes
			}
		}
	}

	// Endpoint or scopes missing: derive both from the issuer.
	if issuerURL != "" && (tokenEndpoint == "" || scopes == "") {
		if derivedEndpoint, _, derivedScopes := ExternalIdpConfigurationFromIssuer(issuerURL, account.ClientID); derivedEndpoint != "" {
			if tokenEndpoint == "" {
				tokenEndpoint = derivedEndpoint
			}
			if scopes == "" {
				scopes = derivedScopes
			}
		}
	}
	return tokenEndpoint, issuerURL, scopes
}

// RefreshToken refreshes the account's access token. For id-bearing accounts it
// serializes per account with double-checked locking: concurrent callers for one
// account collapse to a single IdP POST (the leader refreshes + persists under
// the lock; followers re-read the now-fresh token from config and skip the
// POST). This replaces the refresh-storm + lost-update race that occurred when
// every concurrent request POSTed independently and raced writing back the
// (rotated) refresh token.
//
// Returns: accessToken, refreshToken, expiresAt, profileArn, error.
func RefreshToken(account *config.Account) (string, string, int64, string, error) {
	return refreshTokenWithPolicy(account, false)
}

// RefreshTokenForce refreshes unconditionally: it keeps the per-account lock (so
// concurrent forced refreshes still serialize and a rotated refresh token is
// never lost) but does NOT skip the IdP POST just because the stored token still
// looks valid.
//
// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's refreshAccountToken
// takes a `force` flag and its callers (the admin "refresh" action, the batch
// refresh, and apiTestAccount) rely on a forced refresh actually reaching the
// IdP. The fork's double-checked locking made every such call a silent no-op
// whenever the current token was unexpired, which is exactly what
// TestRefreshAccountTokenSerializesExternalIDPRotation caught: the second forced
// refresh never POSTed, so the rotated refresh token was never exercised.
func RefreshTokenForce(account *config.Account) (string, string, int64, string, error) {
	return refreshTokenWithPolicy(account, true)
}

func refreshTokenWithPolicy(account *config.Account, force bool) (string, string, int64, string, error) {
	if account == nil {
		return "", "", 0, "", fmt.Errorf("RefreshToken: nil account")
	}

	// Id-less accounts (e.g. the login/add-account validation flow builds a
	// temporary account) have no persisted state to coordinate against, so skip
	// the lock + double-check and refresh directly (original behavior).
	if account.ID == "" {
		return refreshTokenDirect(account)
	}

	lock := refreshLockFor(account.ID)
	lock.Lock()
	defer lock.Unlock()

	// Double-checked locking: a concurrent refresh may have renewed the token
	// while we waited. Re-read the canonical expiry from config; if it is still
	// valid, propagate it and skip the IdP POST. Either way adopt the canonical
	// fields (a concurrent refresh may have rotated the refresh token) — that
	// adoption happens even under force, because the whole point of holding the
	// lock is to POST with the newest refresh token rather than a stale copy.
	if live, ok := config.GetAccountByID(account.ID); ok {
		*account = live
		if !force {
			if now := time.Now().Unix(); live.ExpiresAt > 0 && now < live.ExpiresAt-refreshSkewSeconds {
				return live.AccessToken, live.RefreshToken, live.ExpiresAt, live.ProfileArn, nil
			}
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := refreshTokenDirect(account)
	if err != nil {
		return "", "", 0, "", err
	}

	// Persist under the lock so the next caller's double-check sees the fresh
	// token — this is what collapses N concurrent refreshes into one IdP POST.
	// (Existing handler call sites also persist; those writes are now idempotent
	// no-ops and are left untouched per the no-call-site-change constraint.)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
	}
	if perr := config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt); perr != nil {
		logger.Warnf("[RefreshToken] failed to persist refreshed token for %s: %v", account.Email, perr)
	}
	if profileArn != "" {
		if perr := config.UpdateAccountProfileArn(account.ID, profileArn); perr != nil {
			logger.Warnf("[RefreshToken] failed to persist profileArn for %s: %v", account.Email, perr)
		}
	}
	return accessToken, refreshToken, expiresAt, profileArn, nil
}

// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's refreshExternalIdpToken
// and postExternalIdpToken used to live here. Upstream v1.1.5 shipped rival
// implementations of both in auth/microsoft_sso.go — same package, same names,
// different signatures (theirs take an extra issuerURL, and their POST returns a
// *externalIdpTokenResponse instead of three scalars), so keeping both was not
// possible. Upstream's versions are kept because they are a strict superset of
// the behavior this pair had:
//
//   - endpoint allow-list re-validation at the outbound-POST boundary (the
//     fork's exfiltration guard) is preserved via ValidateExternalIdpEndpoint;
//   - plus issuer parsing, tenant-pinning of the token endpoint, scope
//     normalization, a redirect-refusing client, a bounded body read, and an
//     expires_in sanity check, none of which the fork's version had;
//   - the fork's "never echo the response body in an error" redaction property
//     is also honored there: errors carry status + OAuth error code only.
//
// The fork's callers were rewired to the surviving signatures rather than the
// other way around. See externalIdpRefreshInputs above for how accounts stored
// before upstream's validation existed are backfilled instead of being failed.

// refreshOIDCToken IdC/Builder ID token 刷新
func refreshOIDCToken(refreshToken, clientID, clientSecret, region string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || clientSecret == "" {
		return "", "", 0, "", fmt.Errorf("OIDC refresh requires clientId and clientSecret")
	}
	if region == "" {
		region = "us-east-1"
	}
	// Validate the region before it is interpolated into the token endpoint's
	// HOST. This is the most reachable site of that class: the value comes from
	// the stored account and is used automatically by BACKGROUND refresh, with
	// no operator action needed to trigger it. An unvalidated region would send
	// the account's refresh token (a long-lived credential) to whatever host the
	// region injected. See auth/region.go.
	normalizedRegion, ok := validAWSRegionLabel(region)
	if !ok {
		return "", "", 0, "", fmt.Errorf("invalid AWS region %q", region)
	}

	url := oidcTokenURL(normalizedRegion)

	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}

// refreshSocialToken Social (GitHub/Google) token 刷新
func refreshSocialToken(refreshToken string, client *http.Client) (string, string, int64, string, error) {
	url := socialTokenURL()

	payload := map[string]string{
		"refreshToken": refreshToken,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}
