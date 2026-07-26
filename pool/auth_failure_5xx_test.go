package pool

import (
	"errors"
	"testing"
)

// IsAuthFailure decides whether an upstream error means THIS account's
// credentials are dead. classifyAndBanOnUsageError (proxy/kiro_api.go:1266)
// routes a true straight into banAccountInline(..., "BANNED", ...) — a permanent
// ban only an operator can lift, applied from the BACKGROUND refresh loop where
// no human is watching.
//
// The message it inspects embeds an opaque upstream response body: a stack
// trace, a gateway/WAF error page, or an IdP error_description that copies a
// request marker back to the caller. So a 5xx — the upstream failing, not the
// credential being revoked — whose body merely happens to contain "invalid_grant"
// or "unauthorized" used to permanently ban a perfectly healthy account.
//
// A server error cannot revoke a credential. No phrase found inside one is
// evidence about that credential.
func TestIsAuthFailureIgnoresBodyMarkersOn5xx(t *testing.T) {
	outages := []string{
		"HTTP 500 from kiro: {\"error\":\"server_error\",\"error_description\":\"internal trace mentions invalid_grant\"}",
		"HTTP 502 from kiro: <html>bad gateway: unauthorized upstream dependency</html>",
		"HTTP 503 from kiro: service unavailable, token expired in internal cache",
	}
	for _, msg := range outages {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("5xx outage classified as a credential failure (permanent ban):\n  %s", msg)
		}
	}
}

// The 5xx guard must not blunt genuine credential failures: a real 401/403, and
// a marker-only message with no HTTP status at all (OAuth refresh failures are
// surfaced that way), must still classify as auth failures.
func TestIsAuthFailureStillCatchesRealCredentialFailures(t *testing.T) {
	real := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"invalid_grant",
		"bad credentials",
		"unauthorized",
		"HTTP 400 from idp: {\"error\":\"invalid_grant\"}",
	}
	for _, msg := range real {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("real credential failure no longer classified: %q", msg)
		}
	}
}
