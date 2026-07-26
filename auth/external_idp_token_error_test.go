package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// postExternalIdpToken builds the error string that (a) the background refresher
// logs verbatim (proxy/handler.go "Token refresh failed for %s: %v") and (b) the
// ban classifier mines (proxy/account_failover.go isAuthErrorMessage). Both
// consumers make the free-text `error_description` field load-bearing, and that
// field is upstream-controlled. These tests pin the three properties that field
// must not violate.

// An IdP that echoes a submitted parameter back inside error_description writes a
// live long-lived credential into the operator's log file, where it outlives the
// request and is readable by anyone with log access. The values at risk are the
// ones WE sent, so redaction is exact-substring, not a guess about what looks
// secret.
func TestExternalIdpTokenErrorRedactsSubmittedSecrets(t *testing.T) {
	const secret = "REFRESH_TOKEN_MUST_NOT_REACH_LOGS"
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusBadRequest,
			`{"error":"invalid_grant","error_description":"token refresh_token=`+secret+` was revoked"}`), nil
	})}

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {secret}}
	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, form)
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("submitted refresh token leaked into an error that is logged verbatim:\n%s", err.Error())
	}
	// Redaction must not cost the diagnostic value the operator needs.
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("redaction destroyed diagnostic context: %s", err.Error())
	}
}

// A non-JSON error body (WAF block page, gateway HTML) currently fails
// json.Unmarshal BEFORE the status is ever inspected, so a genuine HTTP 401 is
// reported as an unclassified parse error. The ban classifier then sees no status
// and no marker, so a revoked credential is never recognised as one.
func TestExternalIdpTokenErrorKeepsStatusOnUnparseableBody(t *testing.T) {
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusUnauthorized, `<html>denied</html>`), nil
	})}

	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("genuine HTTP 401 lost its status and became unclassifiable: %s", err.Error())
	}
}

// The response body must never be echoed, parseable or not: an unparseable body
// is exactly the shape (proxy pages, stack traces) most likely to carry
// unrelated sensitive material.
func TestExternalIdpTokenErrorDoesNotEchoUnparseableBody(t *testing.T) {
	const marker = "BODY_CONTENT_MUST_NOT_BE_ECHOED"
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusBadGateway, `<html>`+marker+`</html>`), nil
	})}

	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected an error for a 502 response")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("response body echoed into the error: %s", err.Error())
	}
}
