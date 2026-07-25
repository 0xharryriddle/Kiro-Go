package proxy

import "testing"

// Upstream errors are formatted as "HTTP <status> from <endpoint>: <body>"
// (proxy/kiro.go:520), so the error string carries the FULL upstream response
// body. isAuthErrorMessage then substring-matched bare words like "unauthorized"
// and "forbidden" anywhere in that string, and handleAccountFailure routes an
// auth classification to disableAccount(..., "BANNED", ...) — a PERMANENT ban
// that only an operator can undo.
//
// So any unrelated 5xx whose body happens to mention one of those words (an
// upstream stack trace, a WAF page, a JSON error describing some other
// resource's permissions) permanently banned a perfectly healthy account.
//
// The status token is authoritative and always appears in the "HTTP <status>"
// prefix; the body is not. These tests pin that distinction.
func TestUnrelatedServerErrorMentioningAuthWordsIsNotAuthFailure(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			name: "500 whose body narrates an unauthorized downstream call",
			msg:  `HTTP 500 from kiro: {"message":"internal error while checking if caller was unauthorized"}`,
		},
		{
			name: "502 gateway page containing the word Forbidden",
			msg:  "HTTP 502 from kiro: <html><body>Bad Gateway (upstream said Forbidden)</body></html>",
		},
		{
			name: "503 whose body echoes a token expired log line",
			msg:  "HTTP 503 from kiro: service unavailable; last log entry: token expired for unrelated session",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isAuthErrorMessage(c.msg) {
				t.Fatalf("non-auth upstream error classified as an auth failure, which permanently BANS the account:\n%s", c.msg)
			}
		})
	}
}

// Genuine auth failures must still be detected, otherwise revoked credentials
// would keep getting routed forever. This is the guard against over-correcting.
func TestGenuineAuthFailuresStillDetected(t *testing.T) {
	cases := []string{
		"HTTP 401 from kiro: {\"message\":\"Unauthorized\"}",
		"HTTP 403 from kiro: {\"message\":\"The security token included in the request is invalid\"}",
		"refresh failed: invalid_grant",
		"authentication failed for account",
		"access token expired",
		"refresh token expired",
		"token invalid",
	}

	for _, msg := range cases {
		if !isAuthErrorMessage(msg) {
			t.Fatalf("genuine auth failure NOT detected (revoked credentials would keep being routed): %q", msg)
		}
	}
}

// statusForUpstreamError feeds the HTTP status returned to the client, so the
// same misclassification also turned an upstream 500 into a client-facing 401.
func TestStatusForUpstreamErrorDoesNotReport401ForServerError(t *testing.T) {
	err := errString(`HTTP 500 from kiro: {"message":"caller was unauthorized to read the audit log"}`)
	if got := statusForUpstreamError(err); got == 401 {
		t.Fatalf("upstream 500 surfaced to the client as 401 authentication_error")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
