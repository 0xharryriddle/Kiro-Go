package proxy

import (
	"regexp"

	"kiro-go/config"
)

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
	//
	// The key alternation deliberately allows an optional vendor prefix and
	// either separator (x-api-key, api_key, apiKey), because the same secret
	// appears BOTH as an HTTP header name and as a JSON object key depending on
	// where it was captured. An earlier version only handled the header form
	// (`x-api-key: value`) and silently leaked the JSON form
	// (`"x-api-key": "value"`), since the closing quote before the colon
	// defeated the header pattern.
	{regexp.MustCompile(`(?i)"(?:x-)?(access[-_]?token|refresh[-_]?token|id[-_]?token|client[-_]?secret|api[-_]?key|kiro[-_]?api[-_]?key|password|authorization|cookie|secret)"\s*:\s*"[^"]*"`), `"$1":"[REDACTED]"`},
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

// traceDefaultMaxBodyBytes caps a captured body when no explicit limit is set.
const traceDefaultMaxBodyBytes = 256 * 1024

// scrubTraceBody prepares a request/response body for persistence under the
// given capture mode.
//
// Returns nil unless the mode actually permits body capture, so a caller can
// never accidentally persist prompt text while the deployment is configured for
// metadata only. Credential scrubbing is unconditional in the modes that do
// capture: "full" governs how much PROMPT text is kept, never whether secrets
// are kept. "redacted" additionally applies the existing PII redaction used for
// outbound prompts (proxy/translator.go), so the two paths stay consistent.
func scrubTraceBody(raw []byte, mode string) []byte {
	if len(raw) == 0 {
		return nil
	}
	switch mode {
	case config.TraceCaptureRedacted, config.TraceCaptureFull:
	default:
		// "off", "meta", empty, or anything unrecognised: never store bodies.
		return nil
	}

	out := scrubTraceText(string(raw))
	if mode == config.TraceCaptureRedacted {
		out = redactPII(out)
	}
	return []byte(out)
}

// truncateTraceBody caps a body at maxBytes, reporting whether it was cut so the
// record can be flagged and a reader never mistakes a partial body for the whole
// payload. A non-positive maxBytes means "use the default", never "store
// nothing" -- silently discarding on a misconfigured cap would be worse than
// storing a bounded prefix.
func truncateTraceBody(raw []byte, maxBytes int) ([]byte, bool) {
	if maxBytes <= 0 {
		maxBytes = traceDefaultMaxBodyBytes
	}
	if len(raw) <= maxBytes {
		return raw, false
	}
	return raw[:maxBytes], true
}
