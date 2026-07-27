package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// ensureValidToken treats a zero ExpiresAt as "nothing to do":
//
//	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-skew {
//	    return nil          // handler.go:3621
//	}
//
// That is CORRECT for credentials which never expire — Bedrock (static IAM keys
// or a bearer API key), custom_api (a peer pool's key), api_key (ksk_). It is
// WRONG for an OAuth account, whose access token lives about an hour: such an
// account carries a perfectly good refresh token, but because the zero short
// -circuits before refreshAccountToken is reached, neither the request path nor
// the background refresher ever uses it. The account serves until the token
// expires and then fails every request, and those failures drive it through
// handleAccountFailure into cooldown and eventually a ban.
//
// Zero expiry is reachable: the bot supply route validates accessToken,
// refreshToken, authMethod, per-method refresh material and profileArn, but never
// requires a positive expiresAt (admin_bot_api.go:958-1047), and persists the
// account as given.
//
// The distinction this pins: for an account that HAS refresh material, a zero
// expiry must mean "expired, refresh it", not "never expires".
func TestEnsureValidTokenRefreshesOAuthAccountWithZeroExpiry(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	// A social OAuth account: refresh token present, expiry zero.
	acct := &config.Account{
		ID:           "acct-oauth-zero-expiry",
		Enabled:      true,
		AuthMethod:   "social",
		AccessToken:  "expired-access-token",
		RefreshToken: "valid-refresh-token",
		ExpiresAt:    0,
	}

	err := h.ensureValidToken(acct)

	// A refresh MUST have been attempted. There is no network in this test, so a
	// real attempt fails — which is exactly how we can tell the difference
	// between "tried and could not" and "did not try at all". Returning nil
	// means the zero was read as "never expires" and the refresh token was
	// silently ignored.
	if err == nil {
		t.Fatalf("ensureValidToken returned nil for an OAuth account with expiresAt=0: " +
			"the zero was treated as \"never expires\", so the account's refresh " +
			"token is never used. The access token expires within the hour and " +
			"every later request fails, driving the account into cooldown and a ban")
	}
}

// Positive control 1: a Bedrock account has static credentials and no refresh
// concept. Its zero expiry must keep short-circuiting, or every Bedrock request
// starts attempting an impossible refresh.
func TestEnsureValidTokenLeavesBedrockZeroExpiryAlone(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	acct := &config.Account{
		ID:            "acct-bedrock-static",
		Enabled:       true,
		AuthMethod:    "bedrock",
		BedrockAPIKey: "ABSKstaticcredential000000",
		Region:        "us-east-1",
		ExpiresAt:     0,
	}
	if err := h.ensureValidToken(acct); err != nil {
		t.Fatalf("Bedrock account with static credentials must not attempt a refresh: %v", err)
	}
}

// Positive control 2: a custom_api account links to a peer pool with a static
// key; same reasoning as Bedrock.
func TestEnsureValidTokenLeavesCustomApiZeroExpiryAlone(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	acct := &config.Account{
		ID:          "acct-custom-api",
		Enabled:     true,
		AuthMethod:  "custom_api",
		AccessToken: "sk-upstream-mirror",
		ExpiresAt:   0,
	}
	if err := h.ensureValidToken(acct); err != nil {
		t.Fatalf("custom_api account with a static key must not attempt a refresh: %v", err)
	}
}

// Positive control 3: an OAuth account with a FUTURE expiry must still
// short-circuit — the fix must only change the zero case.
func TestEnsureValidTokenLeavesFreshOAuthTokenAlone(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	acct := &config.Account{
		ID:           "acct-oauth-fresh",
		Enabled:      true,
		AuthMethod:   "social",
		AccessToken:  "fresh-access-token",
		RefreshToken: "valid-refresh-token",
		ExpiresAt:    1 << 40, // far future
	}
	if err := h.ensureValidToken(acct); err != nil {
		t.Fatalf("an OAuth account with a valid future expiry must not refresh: %v", err)
	}
}
