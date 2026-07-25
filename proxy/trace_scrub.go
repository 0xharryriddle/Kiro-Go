package proxy

import "regexp"

// Secret-bearing patterns that must never reach a trace record, an exported log
// file, or the admin UI. Upstream error bodies routinely quote the offending
// request material, so scrubbing is applied to every captured string regardless
// of the configured capture mode — including "full", which controls how much
// PROMPT text is retained, never whether credentials are retained.
var traceSecretPatterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	// Upstream Kiro-issued API keys (ksk_...). Keep the prefix so an operator can
	// still tell WHICH kind of credential was involved.
	{regexp.MustCompile(`ksk_[A-Za-z0-9_\-]{4,}`), "ksk_[REDACTED]"},
	// Bearer tokens in Authorization headers or quoted error text.
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{8,}`), "Bearer [REDACTED]"},
	// JSON credential fields, e.g. "refreshToken":"aaa.bbb.ccc".
	{regexp.MustCompile(`(?i)"(access_?token|refresh_?token|id_?token|client_?secret|api_?key|password)"\s*:\s*"[^"]*"`), `"$1":"[REDACTED]"`},
	// Header-style credential lines, e.g. `Authorization: abc123`.
	{regexp.MustCompile(`(?i)\b(authorization|x-api-key|cookie|set-cookie)\s*:\s*[^\s,;]+`), "$1: [REDACTED]"},
	// AWS SSO/OIDC style opaque credentials occasionally echoed in errors.
	{regexp.MustCompile(`(?i)\b(aws(?:\.[a-z0-9]+)*sso[A-Za-z0-9._\-]{16,})`), "[REDACTED_SSO_TOKEN]"},
}

// scrubTraceText removes credential material from an arbitrary captured string.
// It is intentionally conservative: over-redacting an error message is far
// cheaper than persisting a live credential into data/traces.
func scrubTraceText(s string) string {
	if s == "" {
		return s
	}
	for _, p := range traceSecretPatterns {
		s = p.re.ReplaceAllString(s, p.replacement)
	}
	return s
}
