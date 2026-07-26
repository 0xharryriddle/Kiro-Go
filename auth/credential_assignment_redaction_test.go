package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// redactSubmittedSecrets can only remove values THIS client sent. That leaves two
// holes an IdP can drive, both of which end up in a string that the background
// refresher logs verbatim (proxy/handler.go logs "Token refresh failed for %s:
// %v") and that the SSO completion endpoint serializes into a client-facing
// response:
//
//  1. A credential the IdP RETURNS rather than one we submitted. Under refresh
//     token rotation the newly issued token is a different value, so a
//     submitted-values-only redactor cannot match it.
//  2. A submitted credential shorter than the redactor's length floor. The floor
//     exists so a 5-character scope fragment is not shredded out of legitimate
//     prose, but it also means a short authorization code passes through intact.
//
// Both are the same underlying shape: the description names a sensitive
// parameter and then gives its value. Redacting on the PARAMETER NAME closes
// both without needing to know the value.
func TestTokenErrorRedactsReturnedCredentialByParameterName(t *testing.T) {
	const returned = "ROTATED_REFRESH_TOKEN_WE_NEVER_SENT"
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusInternalServerError,
			`{"error":"server_error","error_description":"issued refresh_token=`+returned+` before persistence failed"}`), nil
	})}

	// The submitted refresh token is deliberately a DIFFERENT value, so only
	// name-based redaction can catch the returned one.
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"THE_ONE_WE_SUBMITTED_LONG_ENOUGH"}}
	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, form)
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if strings.Contains(err.Error(), returned) {
		t.Fatalf("IdP-returned refresh token survived into a logged error:\n%s", err)
	}
}

func TestTokenErrorRedactsShortAuthorizationCode(t *testing.T) {
	const shortCode = "sh0rt-c0de"
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusBadRequest,
			`{"error":"invalid_grant","error_description":"rejected code=`+shortCode+`"}`), nil
	})}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {shortCode}}
	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, form)
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if strings.Contains(err.Error(), shortCode) {
		t.Fatalf("short authorization code survived into a client-facing error:\n%s", err)
	}
}

// JSON-shaped assignments must be covered too: an IdP that echoes its own
// request log will quote the parameter.
func TestTokenErrorRedactsQuotedCredentialAssignment(t *testing.T) {
	const secret = "QUOTED_CLIENT_SECRET_VALUE"
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusBadRequest,
			`{"error":"invalid_client","error_description":"bad {\"client_secret\":\"`+secret+`\"} supplied"}`), nil
	})}

	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("quoted client secret survived into an error:\n%s", err)
	}
}

// The redaction must not shred legitimate diagnostics: the OAuth error code and
// prose that merely MENTIONS a parameter (without assigning it) have to survive,
// or an operator loses the ability to tell a revoked token from a bad request.
func TestTokenErrorKeepsProseThatOnlyMentionsParameters(t *testing.T) {
	issuer := testMicrosoftIssuer()
	tokenEndpoint := issuerBase(issuer) + "/" + testMicrosoftTenantID + "/oauth2/v2.0/token"
	client := &http.Client{Transport: microsoftRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return microsoftTextResponse(request, http.StatusBadRequest,
			`{"error":"invalid_grant","error_description":"the refresh_token has expired; request a new code"}`), nil
	})}

	_, err := postExternalIdpToken(client, tokenEndpoint, issuer, url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	msg := err.Error()
	for _, want := range []string{"400", "invalid_grant", "has expired", "request a new code"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("redaction destroyed diagnostic context %q: %s", want, msg)
		}
	}
}
