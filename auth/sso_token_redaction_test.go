package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The SSO device-authorization flow talks to endpoints whose RESPONSE BODIES
// carry credentials:
//
//   - POST /client/register     returns {"clientId":..., "clientSecret":...}
//   - POST /session/device      returns {"token":...}  (the device session token)
//
// On a non-2xx those handlers reported `fmt.Errorf("HTTP %d: %s", status,
// string(respBody))` — the raw body verbatim. That error is not merely logged:
// apiImportSsoToken appends err.Error() to an `errors` slice which is encoded
// straight into the JSON response (proxy/handler.go), so a body containing a
// client secret or session token is handed back to the caller.
//
// This is the same defect class already fixed twice in auth/microsoft_sso.go and
// auth/oidc.go. Fixing it in two files and leaving a third is what allows it to
// keep recurring, so these tests pin the whole file rather than one function.
//
// The status must SURVIVE: an operator needs it to tell "wrong bearer token"
// (401) from "portal is down" (5xx). Redaction that discards diagnostics is a
// different bug, not a fix.

// ssoTestServer stands up a server that answers every path with the given
// status and body, and returns its base URL.
func ssoTestServer(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestRegisterDeviceClientErrorDoesNotEchoBody(t *testing.T) {
	const secret = "CLIENT-SECRET-MUST-NOT-REACH-THE-CALLER"
	base := ssoTestServer(t, http.StatusBadRequest,
		`{"clientId":"cid","clientSecret":"`+secret+`","error":"invalid_client_metadata"}`)

	_, _, err := registerDeviceClient(base, "https://example.awsapps.com/start")
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("client secret echoed into an error that reaches the API response:\n  %s", err)
	}
	// The status must still be reported.
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error lost the HTTP status, so an operator cannot diagnose it:\n  %s", err)
	}
}

func TestGetDeviceSessionTokenErrorDoesNotEchoBody(t *testing.T) {
	const sessionToken = "DEVICE-SESSION-TOKEN-MUST-NOT-LEAK"
	base := ssoTestServer(t, http.StatusUnauthorized,
		`{"token":"`+sessionToken+`","message":"bearer token rejected"}`)

	_, err := getDeviceSessionToken(base, "some-bearer-token")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if strings.Contains(err.Error(), sessionToken) {
		t.Fatalf("device session token echoed into an error that reaches the API response:\n  %s", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error lost the HTTP status:\n  %s", err)
	}
}

// The remaining three body-echoing sites are lower risk (their bodies are not
// known to carry credentials), but they are on the same credential-bearing flow
// and an upstream is free to include anything in an error body. Pinned together
// so the file has one rule rather than a per-function judgement call.
func TestOtherSsoFlowErrorsDoNotEchoBody(t *testing.T) {
	const canary = "UNEXPECTED-SENSITIVE-VALUE-IN-BODY"

	t.Run("startDeviceAuth", func(t *testing.T) {
		base := ssoTestServer(t, http.StatusForbidden, `{"secret":"`+canary+`"}`)
		_, _, _, err := startDeviceAuth(base, "cid", "csecret", "https://example.awsapps.com/start")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("response body echoed into the error:\n  %s", err)
		}
		if !strings.Contains(err.Error(), "403") {
			t.Fatalf("status lost:\n  %s", err)
		}
	})

	t.Run("acceptUserCode", func(t *testing.T) {
		base := ssoTestServer(t, http.StatusBadRequest, `{"secret":"`+canary+`"}`)
		_, err := acceptUserCode(base, "usercode", "devicesession")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("response body echoed into the error:\n  %s", err)
		}
		if !strings.Contains(err.Error(), "400") {
			t.Fatalf("status lost:\n  %s", err)
		}
	})

	t.Run("approveAuth", func(t *testing.T) {
		base := ssoTestServer(t, http.StatusInternalServerError, `{"secret":"`+canary+`"}`)
		err := approveAuth(base, &deviceContextInfo{DeviceContextID: "dc", ClientID: "cid"}, "devicesession")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("response body echoed into the error:\n  %s", err)
		}
		if !strings.Contains(err.Error(), "500") {
			t.Fatalf("status lost:\n  %s", err)
		}
	})
}
