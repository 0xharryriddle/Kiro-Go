package proxy

import (
	"errors"
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
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
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
