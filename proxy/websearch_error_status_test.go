package proxy

import (
	"errors"
	"testing"
)

// The mixed web-search loop reported EVERY MCP search failure as 502/api_error:
//
//	proxy/websearch_loop.go:86   h.sendClaudeError(w, 502, "api_error", ...)
//	proxy/websearch_loop.go:125  h.sendClaudeError(w, 502, "api_error", ...)
//
// while the pure web-search path classifies the identical error correctly
// (proxy/websearch.go:642-650), and the same loop's own UPSTREAM branch
// (websearch_loop.go:61-69) classifies it too. So one MCP 429 became
// 429/rate_limit_error on a plain web_search request and 502/api_error the moment
// web_search was mixed with another tool.
//
// That difference is client-visible and consequential: an Anthropic consumer keys
// its retry policy off error.type, so a rate limit reported as api_error invites an
// immediate retry into an already-exhausted budget instead of a backoff, and an MCP
// auth failure looks like a transient gateway fault rather than "re-authenticate".
//
// This test pins the shared classifier, which is the only way the three sites can
// be made to agree.
func TestWebSearchErrorStatusClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantType string
	}{
		{
			name:     "MCP 429 is a rate limit",
			err:      errors.New("MCP request failed: HTTP 429: Too Many Requests"),
			wantCode: 429,
			wantType: "rate_limit_error",
		},
		{
			name:     "MCP 401 is an auth failure",
			err:      errors.New("MCP request failed: HTTP 401: Unauthorized"),
			wantCode: 401,
			wantType: "authentication_error",
		},
		{
			name:     "anything else stays a gateway error",
			err:      errors.New("MCP request failed: connection reset by peer"),
			wantCode: 502,
			wantType: "api_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotType := webSearchErrorStatus(tc.err)
			if gotCode != tc.wantCode || gotType != tc.wantType {
				t.Fatalf("webSearchErrorStatus(%q) = (%d, %q), want (%d, %q)",
					tc.err, gotCode, gotType, tc.wantCode, tc.wantType)
			}
		})
	}
}

// The classifier must agree with the pure web-search path it was extracted from,
// so the two surfaces cannot drift apart again.
func TestWebSearchErrorStatusMatchesPureSearchPath(t *testing.T) {
	for _, msg := range []string{
		"MCP request failed: HTTP 429: slow down",
		"MCP request failed: HTTP 401: bad token",
		"MCP request failed: HTTP 500: boom",
		"dial tcp: i/o timeout",
	} {
		err := errors.New(msg)

		// Re-derive the pure path's decision inline (websearch.go:642-650).
		wantCode, wantType := 502, "api_error"
		if isAuthErrorMessage(msg) {
			wantCode, wantType = 401, "authentication_error"
		} else if isQuotaErrorMessage(msg) {
			wantCode, wantType = 429, "rate_limit_error"
		}

		gotCode, gotType := webSearchErrorStatus(err)
		if gotCode != wantCode || gotType != wantType {
			t.Fatalf("classifier disagrees with the pure web-search path for %q: "+
				"got (%d, %q), want (%d, %q)", msg, gotCode, gotType, wantCode, wantType)
		}
	}
}
