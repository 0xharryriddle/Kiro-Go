package proxy

import (
	"errors"
	"net/http"
	"testing"
)

// The Claude mid-stream error event hardcoded {"type":"api_error"} for every
// upstream failure, while the OpenAI stream on the same failure classifies via
// errorTypeForOpenAIStatus. That asymmetry is client-visible and wrong in a way
// that matters: an Anthropic SSE consumer keys its retry policy off error.type.
//
//   - a rate-limit reported as api_error invites an immediate retry into an
//     already-exhausted account instead of backing off;
//   - a revoked credential reported as api_error looks transient, so the client
//     retries forever rather than surfacing "re-authenticate";
//   - an oversized request reported as api_error suggests the service failed,
//     when the correct action is to shrink the conversation.
//
// claudeErrorTypeForStatus is the Claude-side counterpart, mapping the
// authoritative upstream status onto Anthropic's documented error-type enum.
func TestClaudeErrorTypeForStatusMapsDocumentedTypes(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusRequestEntityTooLarge, "request_too_large"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusPaymentRequired, "invalid_request_error"},
		{http.StatusInternalServerError, "api_error"},
		{http.StatusBadGateway, "api_error"},
		{http.StatusServiceUnavailable, "overloaded_error"},
		{http.StatusGatewayTimeout, "api_error"},
	}
	for _, c := range cases {
		if got := claudeErrorTypeForStatus(c.status); got != c.want {
			t.Errorf("claudeErrorTypeForStatus(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

// Every value the mapper can produce must be a type Anthropic actually defines.
// Inventing a type is worse than a generic api_error: a client switching on the
// enum falls through to an unknown branch.
func TestClaudeErrorTypeStaysWithinAnthropicEnum(t *testing.T) {
	allowed := map[string]struct{}{
		"invalid_request_error": {},
		"authentication_error":  {},
		"permission_error":      {},
		"not_found_error":       {},
		"request_too_large":     {},
		"rate_limit_error":      {},
		"api_error":             {},
		"overloaded_error":      {},
	}
	// Sweep every status the proxy could plausibly surface, plus nonsense.
	for status := 0; status <= 599; status++ {
		got := claudeErrorTypeForStatus(status)
		if _, ok := allowed[got]; !ok {
			t.Fatalf("claudeErrorTypeForStatus(%d) = %q, which is not an Anthropic error type", status, got)
		}
	}
}

// End to end through the real classifier: a quota error must reach the client as
// rate_limit_error, not api_error.
func TestClaudeErrorTypeFromRealUpstreamErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "quota exhausted",
			err:  errors.New("HTTP 429 from kiro: quota exhausted"),
			want: "rate_limit_error",
		},
		{
			name: "revoked credential",
			err:  errors.New("refresh failed: 401 {\"error\":\"invalid_grant\"}"),
			want: "authentication_error",
		},
		{
			name: "input too long",
			err:  errors.New("HTTP 400 from kiro: input is too long for requested model"),
			want: "invalid_request_error",
		},
		{
			name: "upstream outage",
			err:  errors.New("HTTP 500 from kiro: internal failure"),
			want: "api_error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := claudeErrorTypeForStatus(statusForUpstreamError(c.err))
			if got != c.want {
				t.Fatalf("error %q classified as %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// The stop_reason on a mid-stream abort must remain distinguishable from a
// normal completion. "error" is outside Anthropic's documented stop_reason enum,
// which is deliberate and load-bearing: collapsing it to end_turn would make a
// truncated response look complete, and a client would treat partial output as
// the whole answer. The separate `error` event carries the detail; this constant
// is what lets a consumer notice at all.
func TestMidStreamAbortStopReasonIsDistinguishable(t *testing.T) {
	if claudeStopReasonError == "end_turn" {
		t.Fatal("a mid-stream abort must not report end_turn: truncated output would look complete")
	}
	if claudeStopReasonError == "" {
		t.Fatal("stop_reason must be present so the client can branch on it")
	}
}
