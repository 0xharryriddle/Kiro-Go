package auth

import (
	"strings"
	"testing"
)

// Name-based redaction must not eat real IdP diagnostics. Azure AD writes prose
// like "error code: 50173" and "the code: invalid_grant was already redeemed",
// where the parameter-looking word is English prose followed by a colon, not an
// assignment of a credential.
//
// The `invalid_grant` case is not merely cosmetic: proxy's isAuthErrorMessage
// classifies a revoked refresh token by finding that marker in the error string
// (proxy/account_failover.go authErrorNarrowMarkers). Redacting it turns a
// genuine credential failure into an unclassifiable error, so the account is
// never marked as needing re-auth and keeps being routed. Over-redaction here is
// a functional regression, not a readability nit.
//
// Rule this pins: an `=` delimiter is a machine assignment and is redacted; a
// bare `:` is prose and is NOT, unless the name is JSON-quoted ("name": "value").
func TestRedactionPreservesAzureADProse(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"numeric error code", "AADSTS50173: error code: 50173 the token is invalid"},
		{"status code prose", "status code: 401 returned by tenant"},
		{"oauth marker in prose", "the code: invalid_grant was already redeemed"},
		{"spaced colon", "error code : 700082"},
		{"url after colon", "Provide a valid code: see https://aka.ms/oauth"},
		{"token state prose", "refresh_token: expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactCredentialAssignments(c.in)
			if got != c.in {
				t.Fatalf("prose was redacted:\n  in   %q\n  got  %q", c.in, got)
			}
		})
	}
}

// The marker isAuthErrorMessage depends on must survive end to end.
func TestRedactionKeepsAuthClassificationMarker(t *testing.T) {
	in := "the code: invalid_grant was already redeemed"
	got := redactCredentialAssignments(in)
	if !strings.Contains(got, "invalid_grant") {
		t.Fatalf("redaction destroyed the auth-classification marker: %q", got)
	}
}

// Machine assignments must still be redacted, or the fix is worthless.
func TestRedactionStillCatchesMachineAssignments(t *testing.T) {
	const secret = "SECRET_VALUE_0123456789abcdef"
	cases := []string{
		"refresh_token=" + secret,
		"refresh_token = " + secret,
		"refresh_token\t=\t" + secret,
		`{"refresh_token":"` + secret + `"}`,
		`{"refresh_token" : "` + secret + `"}`,
		"REFRESH_TOKEN=" + secret,
		"client_secret=" + secret,
	}
	for _, in := range cases {
		got := redactCredentialAssignments(in)
		if strings.Contains(got, secret) {
			t.Errorf("machine assignment leaked:\n  in   %q\n  got  %q", in, got)
		}
	}
}

// A submitted credential shorter than the value-based floor must still be
// removed when it appears as a machine assignment — that was the whole point of
// adding name-based redaction alongside the value-based pass.
func TestRedactionStillCatchesShortSubmittedCode(t *testing.T) {
	got := redactCredentialAssignments("rejected code=sh0rt")
	if strings.Contains(got, "sh0rt") {
		t.Fatalf("short code leaked from a machine assignment: %q", got)
	}
}
