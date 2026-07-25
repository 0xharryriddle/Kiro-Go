package proxy

import "testing"

// Not every upstream error is formatted as "HTTP <status> from <endpoint>".
// auth/oidc.go and auth/kiro_sso.go produce "refresh failed: <status> <body>"
// and "... failed (status <status>): <body>", and those errors flow into the same
// handleAccountFailure classifier via proxy/handler.go's refresh paths.
//
// If the status in those forms is not recognised, the classifier falls back to
// scanning the embedded body for bare auth words — the exact behaviour that
// permanently bans a healthy account when an unrelated 5xx body happens to
// contain "unauthorized" or "forbidden".
func TestNonAuthStatusInAlternateErrorFormatsIsNotAuthFailure(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			name: "oidc refresh 500 whose body mentions unauthorized",
			msg:  `refresh failed: 500 {"message":"internal error: caller was unauthorized downstream"}`,
		},
		{
			name: "oidc refresh 502 whose body mentions forbidden",
			msg:  "refresh failed: 502 <html>Bad Gateway: Forbidden by upstream cache</html>",
		},
		{
			name: "social token exchange 503 mentioning token expired",
			msg:  `social token exchange failed (status 503): {"detail":"token expired for another tenant"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isAuthErrorMessage(c.msg) {
				t.Fatalf("non-auth error permanently BANS the account:\n%s", c.msg)
			}
		})
	}
}

// The genuine credential failures in those same formats must still be caught.
func TestGenuineAuthFailuresInAlternateFormatsStillDetected(t *testing.T) {
	cases := []string{
		`refresh failed: 400 {"error":"invalid_grant"}`,
		`refresh failed: 401 {"message":"Unauthorized"}`,
		"social token exchange failed (status 403): forbidden",
		`external IdP token exchange failed (status 400): invalid_grant: refresh token revoked`,
	}

	for _, msg := range cases {
		if !isAuthErrorMessage(msg) {
			t.Fatalf("genuine credential failure NOT detected, revoked account keeps being routed: %q", msg)
		}
	}
}
