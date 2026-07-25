package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postExternalIdpToken treats a 2xx response whose access_token is missing as a
// failure (oidc.go:123) and then interpolates the ENTIRE response body into the
// returned error (oidc.go:127).
//
// An OAuth token response is exactly the wrong thing to stringify: a 2xx body
// that lacks access_token can still legitimately carry refresh_token, and the
// resulting error is logged verbatim by the background refresher
// (proxy/handler.go:433 logs "Token refresh failed for %s: %v"). That writes a
// live long-lived credential into the operator's log file, where it long outlives
// the request and is readable by anyone with log access.
//
// This test drives the REAL function against a real HTTP server, so it proves
// what actually reaches the error string.
func TestExternalIdpTokenErrorDoesNotLeakRefreshToken(t *testing.T) {
	const secret = "SUPER_SECRET_REFRESH_TOKEN_do_not_log"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 OK, but no access_token: hits the "missing token" failure path
		// while still carrying a real secret in the body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"refresh_token":"` + secret + `","token_type":"Bearer"}`))
	}))
	defer server.Close()

	// Relax the endpoint allow-list for the httptest host via the documented seam.
	restore := SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer SetExternalIdpValidatorForTest(restore)

	_, _, _, err := postExternalIdpToken(server.Client(), server.URL, url.Values{})
	if err == nil {
		t.Fatal("expected an error when access_token is absent")
	}

	if strings.Contains(err.Error(), secret) {
		t.Fatalf("refresh token leaked into an error that gets logged verbatim:\n%s", err.Error())
	}
}

// The error must still be actionable: an operator needs the status and the
// upstream error code to diagnose the failure. This guards against "fixing" the
// leak by discarding all diagnostic context.
func TestExternalIdpTokenErrorKeepsDiagnosticContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
	}))
	defer server.Close()

	restore := SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer SetExternalIdpValidatorForTest(restore)

	_, _, _, err := postExternalIdpToken(server.Client(), server.URL, url.Values{})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") {
		t.Fatalf("error lost the HTTP status: %s", msg)
	}
	if !strings.Contains(msg, "invalid_grant") {
		t.Fatalf("error lost the upstream error code: %s", msg)
	}
}
