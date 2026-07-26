package proxy

import (
	"errors"
	"net/http"
	"testing"
)

// isAuthErrorMessage decides two things at once: whether to permanently BAN the
// account, and (via statusForUpstreamError) what HTTP status the customer sees.
// Gating 5xx out of the auth classification therefore has a second, client-facing
// consequence that deserves its own pin: an upstream server error must surface as
// a gateway error, NOT as 401.
//
// Why that is the correct answer rather than an unfortunate side effect: 401 tells
// the caller "your credentials are wrong, stop retrying and re-authenticate". When
// the truth is "the upstream is broken", that instruction is actively harmful —
// the client rotates keys or halts a working integration over an outage it should
// simply retry. 502 says "upstream failed, this may be transient", which is the
// honest signal.
func TestUpstream5xxWithAuthMarkerIsNotReportedAs401(t *testing.T) {
	cases := []string{
		`HTTP 500 from kiro: {"error":"server_error","error_description":"internal trace mentions invalid_grant"}`,
		`HTTP 502 from kiro: <html>bad gateway: unauthorized upstream dependency</html>`,
		`HTTP 503 from kiro: service unavailable, token expired in internal cache`,
		`upstream returned 500: authentication failed for an internal dependency`,
	}
	for _, msg := range cases {
		if got := statusForUpstreamError(errors.New(msg)); got == http.StatusUnauthorized {
			t.Errorf("upstream 5xx reported to the client as 401 (credentials blamed for an outage):\n  %s", msg)
		}
	}
}

// The complement: a GENUINE credential failure must still reach the client as
// 401, or a revoked token looks like a transient outage the client keeps retrying
// forever. This is the guard that stops the fix above from being "return 502 for
// everything".
func TestGenuineAuthFailureStillReportedAs401(t *testing.T) {
	cases := []string{
		`HTTP 401 from kiro: {"message":"Unauthorized"}`,
		`HTTP 403 from kiro: {"message":"Forbidden"}`,
		`refresh failed: 400 {"error":"invalid_grant"}`,
		`external IdP token exchange failed (status 400): invalid_grant: refresh token revoked`,
	}
	for _, msg := range cases {
		if got := statusForUpstreamError(errors.New(msg)); got != http.StatusUnauthorized {
			t.Errorf("genuine credential failure reported as %d, want 401:\n  %s", got, msg)
		}
	}
}
