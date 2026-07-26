package proxy

import "testing"

// isAuthErrorMessage decides whether an upstream failure means THIS account's
// credentials are bad, and a true answer routes to disableAccount(..., "BANNED",
// ...) — a permanent ban only an operator can lift. Accounts are scarce and a ban
// is effectively unrecoverable, so a false positive is the most expensive
// classification error this proxy can make.
//
// The function already learned once (see its comment) that mining the embedded
// response body for bare words permanently banned healthy accounts. That fix
// kept a narrow-marker vote for any definite non-auth status, which leaves 5xx
// exposed: an upstream outage whose body merely MENTIONS a grant condition (a
// stack trace, a trace ID, an internal message quoting invalid_grant) still bans
// the account. A 5xx is a server-side outage; it is never a verdict about a
// credential, so the body must get no vote there.
func TestAuthErrorClassificationIgnoresBodyMarkersOn5xx(t *testing.T) {
	cases := []string{
		// The exact shape auth.postExternalIdpToken produces when an IdP 500s
		// with a description that quotes an OAuth error code.
		"HTTP 500: server_error: internal trace mentions invalid_grant",
		"external IdP token exchange failed (status 502): bad gateway invalid_grant",
		"upstream returned 503: authentication failed for internal dependency",
	}
	for _, msg := range cases {
		if isAuthErrorMessage(msg) {
			t.Errorf("5xx outage classified as a credential failure (permanent ban):\n  %s", msg)
		}
	}
}

// The 5xx carve-out must not weaken the classifications that are genuine, so
// these stay true: 401/403 at any position, and 4xx carrying a real grant error.
func TestAuthErrorClassificationStillCatchesRealAuthFailures(t *testing.T) {
	cases := []string{
		"HTTP 401: invalid_token",
		"upstream returned 403 forbidden",
		"HTTP 400: invalid_grant: refresh token revoked",
		"refresh failed: invalid_grant",
		"external IdP token exchange failed (status 400): invalid_grant: expired",
	}
	for _, msg := range cases {
		if !isAuthErrorMessage(msg) {
			t.Errorf("genuine credential failure no longer classified as one:\n  %s", msg)
		}
	}
}
