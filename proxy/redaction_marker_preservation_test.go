package proxy

import (
	"strings"
	"testing"

	"kiro-go/auth"
)

// Credential redaction must never destroy the diagnostic markers that
// isAuthErrorMessage classifies a revoked credential by.
//
// This is a CROSS-PACKAGE invariant and that is why the test lives here: the
// redactor is in auth/, the classifier is in proxy/, and neither package's own
// tests can see the coupling. Redaction runs first (on the IdP's
// error_description), then the resulting string is what the classifier reads.
//
// The failure mode is silent and expensive. "code" is a sensitive parameter
// name, so `error code=invalid_grant` looked like a credential assignment and
// the marker was consumed as the value. A genuinely revoked refresh token then
// classified as a non-auth error: the account was never flagged for re-auth and
// kept being routed while every request failed.
//
// This is the same regression class already fixed once for the colon form —
// reached through the `=` form instead. Fixing one shape and not the other is
// what made a second occurrence possible, so this test pins BOTH.
// isNarrowAuthMarker reports whether a marker is one the classifier honours
// under an authoritative 4xx status. Reads the production list directly rather
// than duplicating it, so the two cannot drift.
func isNarrowAuthMarker(marker string) bool {
	for _, m := range authErrorNarrowMarkers {
		if m == marker {
			return true
		}
	}
	return false
}

func TestRedactionPreservesAuthClassificationMarkers(t *testing.T) {
	markers := []string{"invalid_grant", "invalid_token", "unauthorized"}

	descriptions := []string{
		// The `=` form: a space is not a parameter-name byte, so the
		// left-boundary check alone does not stop "error code=" matching "code".
		"error code=invalid_grant",
		"authorization failed, error code=invalid_grant, retry",
		"error code=unauthorized",
		"error code=invalid_token",
		// The JSON-quoted form, which reaches the same branch via quotedKey.
		`{"code":"invalid_grant","message":"revoked"}`,
		`upstream said {"code":"invalid_token"}`,
		// Prose forms that must obviously survive.
		"the refresh_token is invalid_grant",
		"status: unauthorized",
	}

	for _, desc := range descriptions {
		redacted := auth.RedactOAuthDescriptionForTest(desc)
		for _, m := range markers {
			if !strings.Contains(desc, m) {
				continue
			}
			if !strings.Contains(redacted, m) {
				t.Errorf("redaction destroyed the %q marker that isAuthErrorMessage depends on:\n  in   %q\n  out  %q", m, desc, redacted)
				continue
			}
			// And the end-to-end property: the classifier must still see it.
			//
			// Only the NARROW markers are checked here. Under an authoritative
			// 4xx, isAuthErrorMessage honours authErrorNarrowMarkers only —
			// bare "unauthorized" is deliberately excluded there, because a 400
			// whose body merely contains that word is not evidence the
			// credential was revoked (that exclusion is itself a fix, see
			// proxy/account_failover.go). Asserting on it here would be testing
			// the classifier's policy, not the redactor's contract, and would
			// fail for a reason that has nothing to do with redaction.
			if !isNarrowAuthMarker(m) {
				continue
			}
			full := "HTTP 400: invalid_request: " + redacted
			if !isAuthErrorMessage(full) {
				t.Errorf("classifier no longer recognises a credential failure after redaction:\n  in   %q\n  out  %q", desc, full)
			}
		}
	}
}

// The narrowing must not reopen the leak it was added to close: a real
// credential VALUE assigned to a sensitive parameter still has to be removed.
func TestRedactionStillRemovesRealCredentialValues(t *testing.T) {
	const secret = "ROTATED-SECRET-VALUE-0123456789"
	cases := []string{
		"refresh_token=" + secret,
		"client_secret=" + secret,
		"refreshToken=" + secret,
		`{"refresh_token":"` + secret + `"}`,
		"code=" + secret,
		`{"code":"` + secret + `"}`,
	}
	for _, in := range cases {
		out := auth.RedactOAuthDescriptionForTest(in)
		if strings.Contains(out, secret) {
			t.Errorf("credential survived redaction:\n  in   %q\n  out  %q", in, out)
		}
	}
}
