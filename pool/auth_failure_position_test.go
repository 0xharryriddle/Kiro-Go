package pool

import (
	"errors"
	"testing"
)

// The 5xx gate in IsAuthFailure must anchor on the AUTHORITATIVE status — the
// one the formatter put at the front of the message — not on any 5xx-looking
// token anywhere in it.
//
// Every error formatter in this repo writes "<status> <body>": the status is
// structural, the body is opaque upstream text that routinely quotes other
// statuses (a trace, a gateway page, a nested error). Scanning the whole string
// for any 5xx token therefore lets the BODY override the HEADER.
//
// The consequence is the mirror image of the bug the gate was added to fix: a
// genuinely revoked refresh token reported as
//
//	refresh failed: 400 {"error":"invalid_grant","upstream returned 500"}
//
// was classified as a server outage, so classifyAndBanOnUsageError never marked
// the account as needing re-authentication and the pool kept routing to a
// credential that can no longer work.
//
// proxy/account_failover.go's isAuthErrorMessage resolves this by taking the
// LEFTMOST status match; this pins the same rule on the pool side so the two
// classifiers cannot disagree about the same message.
func TestIsAuthFailureAnchorsOnLeadingStatusNotBodyTokens(t *testing.T) {
	authoritative4xx := []string{
		`refresh failed: 400 {"error":"invalid_grant","upstream returned 500"}`,
		`refresh failed: 400 invalid_grant (trace: HTTP 503 from edge)`,
		`HTTP 400: invalid_grant: upstream returned 500 earlier, token revoked`,
	}
	for _, msg := range authoritative4xx {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("revoked credential misread as a server outage because the body quoted a 5xx:\n  %s", msg)
		}
	}
}

// The behaviour the gate exists for must survive: when the AUTHORITATIVE status
// is 5xx, no marker found inside the body counts as evidence about the
// credential.
func TestIsAuthFailureStillIgnoresMarkersUnderLeading5xx(t *testing.T) {
	genuine5xx := []string{
		`HTTP 500 from kiro: {"error":"server_error","error_description":"internal trace mentions invalid_grant"}`,
		`HTTP 502 from kiro: <html>bad gateway: unauthorized upstream dependency</html>`,
		`HTTP 503 from kiro: service unavailable, token expired in internal cache`,
	}
	for _, msg := range genuine5xx {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("5xx outage classified as a credential failure (permanent ban):\n  %s", msg)
		}
	}
}

// And a genuine 401/403 is still caught regardless of body content.
func TestIsAuthFailureStillCatchesLeading401(t *testing.T) {
	for _, msg := range []string{
		`HTTP 401 from kiro: {"note":"HTTP 500 retry"}`,
		`social token exchange failed (status 403): http 500 inside`,
		"HTTP 401",
	} {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("genuine credential failure not detected:\n  %s", msg)
		}
	}
}
