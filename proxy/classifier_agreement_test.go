package proxy

import (
	"errors"
	"testing"

	"kiro-go/pool"
)

// Two independent auth classifiers exist and BOTH can permanently ban an
// account: proxy's isAuthErrorMessage (via handleAccountFailure ->
// disableAccount(BANNED)) and pool.IsAuthFailure (via
// classifyAndBanOnUsageError -> banAccountInline(BANNED) in kiro_api.go).
//
// They are separate implementations over the same error strings, so they can
// drift apart silently — and drift in either direction is a real defect:
//
//   - proxy says auth / pool says not: the request path re-auths the account
//     while the background refresh keeps treating it as healthy, so the account
//     flaps instead of being flagged.
//   - pool says auth / proxy says not: the background path permanently bans an
//     account the request path considers fine.
//
// This test does not assert a particular verdict; it asserts the two agree.
// Every string below comes from a real formatter in this repo (proxy/kiro.go,
// auth/oidc.go, auth/kiro_sso.go, auth/microsoft_sso.go), including the
// mixed-status shapes where a header status and a body-quoted status disagree —
// which is exactly where they diverged before both were anchored on the
// leftmost/first status token.
func TestAuthClassifiersAgreeOnRealFormatterStrings(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		// Genuine credential failures.
		{"ms_sso 401 invalid_grant", `HTTP 401: invalid_grant: The refresh token has expired`},
		{"ms_sso 400 invalid_grant", `HTTP 400: invalid_grant: token revoked`},
		{"oidc refresh 401", `refresh failed: 401 {"error":"invalid_grant"}`},
		{"oidc refresh 400 invalid_grant", `refresh failed: 400 {"error":"invalid_grant"}`},
		{"kiro_sso social 401", `social token exchange failed (status 401): {"reason":"invalid_grant"}`},
		{"bare 401 no body", `external IdP token endpoint returned HTTP 401`},
		{"bare 403 no body", `external IdP token endpoint returned HTTP 403`},
		{"bare 401 only", `HTTP 401`},

		// Genuine upstream outages that merely mention an auth marker: neither
		// classifier may read these as a revoked credential.
		{"5xx body mentions invalid_grant", `HTTP 500 from kiro: <html>stack: invalid_grant handler</html>`},
		{"502 gateway says forbidden", `refresh failed: 502 <html>Bad Gateway: Forbidden by cache</html>`},
		{"503 body says token expired", `HTTP 503 from kiro: service unavailable, token expired in internal cache`},

		// Mixed statuses: authoritative header vs a status quoted in the body.
		{"401 header + 503 in body", `refresh failed: 401 {"error":"invalid_grant","trace":"HTTP 503 from edge"}`},
		{"400 invalid_grant + 500 in body", `refresh failed: 400 {"error":"invalid_grant","upstream returned 500"}`},
		{"401 desc echoes 500", `HTTP 401: invalid_grant: upstream returned 500 earlier, token revoked`},
		{"kiro 401 with 5xx in body", `HTTP 401 from https://q.example/v1: {"msg":"denied","note":"HTTP 500 retry"}`},
		{"status-form 401 with 5xx inside", `social token exchange failed (status 401): http 500 inside`},

		// No status at all: marker-only paths must also agree.
		{"marker only, no status", `refresh failed: invalid_grant`},
		{"no marker, no status", `dial tcp: connection refused`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proxyVerdict := isAuthErrorMessage(c.msg)
			poolVerdict := pool.IsAuthFailure(errors.New(c.msg))
			if proxyVerdict != poolVerdict {
				t.Fatalf("classifiers disagree (both can permanently ban):\n  msg   %q\n  proxy.isAuthErrorMessage = %v\n  pool.IsAuthFailure       = %v",
					c.msg, proxyVerdict, poolVerdict)
			}
		})
	}
}

// The agreement above must not be achieved by both classifiers answering the
// same way to everything. These pin the two verdicts that actually matter, so a
// future "fix" that makes one classifier constant would fail here rather than
// silently satisfying the agreement test.
func TestAuthClassifiersDiscriminateRealCases(t *testing.T) {
	revoked := `refresh failed: 401 {"error":"invalid_grant"}`
	outage := `HTTP 500 from kiro: <html>stack: invalid_grant handler</html>`

	if !isAuthErrorMessage(revoked) || !pool.IsAuthFailure(errors.New(revoked)) {
		t.Fatal("a revoked credential must be classified as an auth failure by both")
	}
	if isAuthErrorMessage(outage) || pool.IsAuthFailure(errors.New(outage)) {
		t.Fatal("a 5xx outage must NOT be classified as an auth failure by either")
	}
}
