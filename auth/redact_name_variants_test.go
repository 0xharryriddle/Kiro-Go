package auth

import (
	"net/url"
	"strings"
	"testing"
)

// The redactor matched only exact snake_case parameter names, but this codebase's
// own upstream speaks camelCase: auth/oidc.go:291-292 and config/config.go:65-66
// declare the fields as `json:"refreshToken"` / `json:"accessToken"`. So the one
// spelling most likely to appear in an echoed upstream body was the one spelling
// not covered.
//
// That matters because the value in a rotated-credential echo is NOT a value we
// submitted, so the value-based pass cannot catch it either — name matching is
// the only control, and it was blind to the shape upstream actually emits.
func TestRedactionCoversCamelCaseAndHyphenatedNames(t *testing.T) {
	const secret = "ROTATED-SECRET-VALUE-0123456789"
	cases := []string{
		"refreshToken=" + secret,
		"accessToken=" + secret,
		"idToken=" + secret,
		"clientSecret=" + secret,
		"refresh-token=" + secret,
		"access-token=" + secret,
		"client-secret=" + secret,
		`{"refreshToken":"` + secret + `"}`,
		`{"accessToken": "` + secret + `"}`,
	}
	for _, in := range cases {
		got := redactCredentialAssignments(in)
		if strings.Contains(got, secret) {
			t.Errorf("credential leaked for a name spelling upstream actually uses:\n  in   %q\n  got  %q", in, got)
		}
	}
}

// Redaction must be idempotent in shape: the two passes run back to back
// (redactSubmittedSecrets substitutes the placeholder, then calls the
// assignment pass over its own output), so the assignment pass must not mangle a
// placeholder the value pass just wrote.
//
// It did: ']' was a value terminator but '[' was not, so the value scan stopped
// before the closing bracket of "[REDACTED]" and the orphan ']' was re-emitted,
// yielding "[REDACTED]]". No secret escapes, but a redaction routine that
// corrupts its own output invites doubt about what else it rewrote.
func TestRedactionDoesNotMangleItsOwnPlaceholder(t *testing.T) {
	form := url.Values{}
	form.Set("refresh_token", "LIVE-REFRESH-TOKEN-0123456789")

	got := redactSubmittedSecrets("token refresh_token=LIVE-REFRESH-TOKEN-0123456789 was revoked", form)
	if strings.Contains(got, "[REDACTED]]") {
		t.Fatalf("placeholder was mangled by the second pass: %q", got)
	}
	if strings.Contains(got, "LIVE-REFRESH-TOKEN-0123456789") {
		t.Fatalf("secret survived: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("nothing was redacted: %q", got)
	}

	// Applying the assignment pass again must be a no-op on an already-redacted
	// string — otherwise repeated logging grows brackets without bound.
	twice := redactCredentialAssignments(got)
	if twice != got {
		t.Fatalf("assignment pass is not idempotent:\n  once  %q\n  twice %q", got, twice)
	}
}

// Guard the other direction: broadening the name list must not start eating
// prose. These mention credential-ish words without assigning a value.
func TestRedactionStillPreservesProseWithVariantNames(t *testing.T) {
	cases := []string{
		"the refreshToken has expired",
		"accessToken is missing from the response",
		"error code: 50173 the token is invalid",
		"the code: invalid_grant was already redeemed",
	}
	for _, in := range cases {
		if got := redactCredentialAssignments(in); got != in {
			t.Errorf("prose was redacted:\n  in   %q\n  got  %q", in, got)
		}
	}
}
