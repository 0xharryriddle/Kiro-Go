package proxy

import (
	"errors"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

// TestProfileAuthz403NotClassifiedAsTokenBan locks in that the live Kiro IDE
// error string — "HTTP 403 ... User is not authorized to make this call." — is
// treated as a profile/plan authorization error (soft, no ban) and NOT as a
// token auth failure. handleAccountFailure checks isProfileOrPlanAuthzError
// BEFORE isAuthErrorMessage, so a missing/unresolved profile no longer
// permanently bans an otherwise-valid Enterprise account.
func TestProfileAuthz403NotClassifiedAsTokenBan(t *testing.T) {
	const liveMsg = `HTTP 403 from Kiro IDE: {"message":"User is not authorized to make this call.","reason":null}`
	if !isProfileOrPlanAuthzError(liveMsg) {
		t.Fatalf("expected profile/plan authz classification for %q", liveMsg)
	}
	// isAuthErrorMessage also matches it (it contains "403"), which is exactly
	// why ordering in handleAccountFailure matters — the profile-authz case must
	// come first so this does not fall through to the permanent-ban branch.
	if !isAuthErrorMessage(liveMsg) {
		t.Fatalf("sanity: expected the broad auth classifier to also match %q", liveMsg)
	}
	// A genuine token failure must still NOT be swallowed by the profile-authz
	// discriminator (so it still reaches the ban branch).
	if isProfileOrPlanAuthzError("HTTP 403: token invalid or expired") {
		t.Fatal("token-invalid 403 must not be classified as a profile/plan error")
	}
}

func TestStatusForUpstreamErrorMapsQuotaTo429(t *testing.T) {
	status := statusForUpstreamError(errors.New("HTTP 429 from Kiro IDE: quota exhausted"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", status)
	}
}

func TestApplyRetryAfterHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	applyRetryAfterHeader(rec, errors.New("HTTP 429 from Kiro IDE: quota exhausted; retry after 120"))
	if got := rec.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("expected Retry-After 120, got %q", got)
	}
}

// TestHandleAccountFailureDoesNotBanOnForbiddenInBody verifies the request-path
// error classifier does NOT permanently ban a healthy account when a NON-auth
// upstream error (e.g. a 502 gateway / CloudFront HTML page) merely contains the
// word "forbidden" in its body. The old isAuthErrorMessage ran bare
// strings.Contains over the full error string (which embeds the upstream body)
// and false-banned healthy accounts. Routing through pool.IsAuthFailure matches
// status codes by digit boundary and uses curated markers, so a body word with no
// 401/403 token and no auth marker must not ban.
func TestHandleAccountFailureDoesNotBanOnForbiddenInBody(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// "forbidden" appears only as a body word — NO 401/403 status token, NO auth
	// marker. Bare substring matching banned this; pool.IsAuthFailure must not.
	h.handleAccountFailure(&acc, errors.New("upstream returned 502: <html>nginx error: access forbidden</html>"))

	got, _ := config.GetAccountByID("acct")
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("account should NOT be banned for a forbidden-in-body non-auth error; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}

// TestHandleAccountFailureBansOnGenuineAuthError verifies a genuine auth failure
// (401 status) still permanently bans the account after routing through
// pool.IsAuthFailure.
func TestHandleAccountFailureBansOnGenuineAuthError(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	h.handleAccountFailure(&acc, errors.New("upstream error (status 401): unauthorized"))

	got, _ := config.GetAccountByID("acct")
	if got.Enabled || got.BanStatus != "BANNED" {
		t.Fatalf("genuine auth error should ban the account; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}

// TestQuotaClassifierStatusTokenBoundary verifies isQuotaErrorMessage matches a
// genuine "429" status code but NOT a stray "429" substring embedded in an
// upstream body token/ID — parity with pool.HasStatusToken (the auth-classifier
// hardening in 3276727). Before routing the digit through HasStatusToken, a body
// like "request 1429abc failed" false-triggered RecordError(true) → soft cooldown.
func TestQuotaClassifierStatusTokenBoundary(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{name: "genuine status", msg: "HTTP 429: quota exhausted", want: true},
		{name: "word marker", msg: "usage quota exceeded", want: true},
		{name: "stray 429 in token", msg: "request id 1429abc failed", want: false},
		{name: "stray 429 in hex id", msg: "trace 4290f3: upstream unavailable", want: false},
	}
	for _, tc := range tests {
		if got := isQuotaErrorMessage(tc.msg); got != tc.want {
			t.Fatalf("isQuotaErrorMessage(%q) = %v, want %v [%s]", tc.msg, got, tc.want, tc.name)
		}
	}
}

// TestOverageClassifierStatusTokenBoundary verifies isOverageErrorMessage
// requires "402" as a digit-boundary token (parity with pool.HasStatusToken), so
// a stray "402" inside a body token can't combine with the word "overage" to
// false-fire disableAccountOverage. The genuine upstream format from
// upstreamError(402,…) — "HTTP 402 overage from …" — still matches.
func TestOverageClassifierStatusTokenBoundary(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{name: "genuine upstream format", msg: "HTTP 402 overage from primary: Usage limit exceeded", want: true},
		{name: "stray 402 in token", msg: "overage noted in region abc402xyz", want: false},
	}
	for _, tc := range tests {
		if got := isOverageErrorMessage(tc.msg); got != tc.want {
			t.Fatalf("isOverageErrorMessage(%q) = %v, want %v [%s]", tc.msg, got, tc.want, tc.name)
		}
	}
}

// TestShouldRetryAccountRefreshOnErrorStatusTokenBoundary verifies the admin
// refresh retry trigger matches genuine 401/403 status codes by digit boundary
// (parity with pool.HasStatusToken used by the ban classifiers), so a stray
// "401"/"403" inside an upstream token/ID can't cause a spurious token-refresh +
// retry. Word markers "invalid"/"expired" are unchanged. This is a RETRY trigger
// (handler.go refresh-account endpoint), not a ban classifier.
func TestShouldRetryAccountRefreshOnErrorStatusTokenBoundary(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{name: "genuine 401", msg: "HTTP 401 from primary: unauthorized", want: true},
		{name: "genuine 403", msg: "upstream error (status 403): forbidden", want: true},
		{name: "token expired", msg: "token expired", want: true},
		{name: "invalid grant", msg: "invalid_grant from idp", want: true},
		{name: "stray 401 in token", msg: "trace 14013ab failed", want: false},
		{name: "stray 403 in hex id", msg: "request 4030f3 timed out", want: false},
		{name: "unrelated error", msg: "dial tcp: connection refused", want: false},
	}
	for _, tc := range tests {
		if got := shouldRetryAccountRefreshOnError(tc.msg); got != tc.want {
			t.Fatalf("shouldRetryAccountRefreshOnError(%q) = %v, want %v [%s]", tc.msg, got, tc.want, tc.name)
		}
	}
}
