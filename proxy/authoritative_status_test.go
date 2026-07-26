package proxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The status that decides whether an account's credentials are bad is the one
// THIS codebase's formatters emitted, and every one of them puts it at the FRONT
// of the message, ahead of the opaque upstream body:
//
//	"refresh failed: 401 <body>"                      auth/oidc.go
//	"HTTP 401 from kiro: <body>"                      proxy/kiro.go
//	"social token exchange failed (status 401): ..."  auth/kiro_sso.go
//	"upstream returned 502: <body>"                   generic gateway errors
//
// Everything after it is attacker/upstream-controlled text that can itself
// contain a status-shaped token — a trace ID, a nested error, an HTML gateway
// page quoting another hop.
//
// upstreamStatusFromMessage picked the first PATTERN that matched rather than
// the earliest match in the STRING, so a genuine 401 whose body mentioned a 5xx
// resolved to the 5xx. Combined with the 5xx gate (which correctly refuses to
// ban on server errors), that silently stopped classifying real revoked
// credentials: the account was never flagged for re-auth and kept being routed
// into guaranteed failures.
func TestAuthoritativeStatusIsTheEarliestInTheMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
	}{
		{
			name: "401 header with a 5xx quoted in the body",
			msg:  `refresh failed: 401 {"error":"invalid_grant","trace":"HTTP 503 from edge"}`,
			want: 401,
		},
		{
			name: "kiro_sso status form with a 5xx inside",
			msg:  "social token exchange failed (status 401): http 500 inside",
			want: 401,
		},
		{
			name: "bare http form with a later 5xx",
			msg:  "refresh failed: 401 body http 503",
			want: 401,
		},
		{
			name: "genuine 5xx keeps its own status",
			msg:  "HTTP 500 from kiro: <html>stack: invalid_grant handler</html>",
			want: 500,
		},
		{
			name: "genuine 5xx that mentions a 401 downstream",
			msg:  "HTTP 502 from kiro: upstream said 401 earlier",
			want: 502,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := upstreamStatusFromMessage(strings.ToLower(c.msg))
			if !ok {
				t.Fatalf("no status found in %q", c.msg)
			}
			if got != c.want {
				t.Fatalf("authoritative status = %d, want %d\n  msg %q", got, c.want, c.msg)
			}
		})
	}
}

// The consequence that actually matters: a revoked credential reported with a
// 401 must still be classified as an auth failure even when its body quotes a
// 5xx, and must still surface to the client as 401.
func TestRevokedCredentialStillClassifiedWhenBodyQuotes5xx(t *testing.T) {
	msg := `refresh failed: 401 {"error":"invalid_grant","trace":"HTTP 503 from edge"}`
	if !isAuthErrorMessage(msg) {
		t.Fatal("a 401 invalid_grant whose body quotes a 5xx was not classified as an auth failure")
	}
	if got := statusForUpstreamError(errors.New(msg)); got != http.StatusUnauthorized {
		t.Fatalf("client status = %d, want 401", got)
	}
}

// And the gate it must not undo: a real 5xx outage still never bans, even when
// its body mentions a credential marker.
func TestGenuine5xxStillDoesNotBan(t *testing.T) {
	for _, msg := range []string{
		"HTTP 500 from kiro: <html>stack: invalid_grant handler</html>",
		"refresh failed: 502 <html>Bad Gateway: Forbidden by cache</html>",
		"HTTP 502 from kiro: upstream said 401 earlier",
	} {
		if isAuthErrorMessage(msg) {
			t.Errorf("5xx outage classified as a credential failure:\n  %s", msg)
		}
	}
}
