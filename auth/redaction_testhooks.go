package auth

import "net/url"

// RedactOAuthDescriptionForTest exposes the credential-redaction pass to tests in
// OTHER packages.
//
// It exists because the redactor has a cross-package contract that neither
// package's own tests can express: auth/ decides what to strip from an IdP
// error_description, and proxy/isAuthErrorMessage then classifies the resulting
// string. Over-redaction in auth/ silently breaks credential detection in
// proxy/, so the invariant has to be pinned from proxy/ where both halves are
// visible.
//
// Test-only. Nothing in production calls this: the real path is
// redactSubmittedSecrets, which also strips the values submitted in the request
// form. This wrapper covers the name-based pass with an empty form, which is
// exactly the shape that operates on values the IdP invented.
func RedactOAuthDescriptionForTest(description string) string {
	return redactSubmittedSecrets(description, url.Values{})
}
