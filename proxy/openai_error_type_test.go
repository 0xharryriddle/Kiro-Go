package proxy

import (
	"errors"
	"net/http"
	"testing"
)

// errorTypeForOpenAIStatus labels every status except 401 and 429 as
// "server_error". That mislabels CLIENT faults as SERVER faults, which is the
// mirror image of the asymmetry claudeErrorTypeForStatus was introduced to
// remove.
//
// Why it matters, in the same terms as the Claude fix: an OpenAI-compatible
// consumer branches on error.type to decide what to do next.
//
//   - "server_error" means "the service failed, retry the same request". For a
//     400 (malformed / oversized request) retrying the identical payload can
//     never succeed, so the client loops until it gives up.
//   - For a 402 (spend cap / overage) it is worse than useless: the correct
//     client action is to fix billing or plan, and no amount of retrying an
//     unchanged request clears a spend cap.
//
// OpenAI's documented error types include "invalid_request_error" for exactly
// this class, so there is a correct value available — the function simply never
// returned it.
//
// Both statuses are reachable: statusForUpstreamError returns 400 for an
// input-too-long error and 402 for an overage error, and that status feeds this
// function at handler.go's OpenAI error sites.
func TestOpenAIErrorTypeDistinguishesClientFaults(t *testing.T) {
	cases := []struct {
		status int
		want   string
		why    string
	}{
		{http.StatusBadRequest, "invalid_request_error",
			"a malformed/oversized request is not a server failure; retrying it unchanged cannot succeed"},
		{http.StatusPaymentRequired, "invalid_request_error",
			"a spend cap is a client-side condition; retrying cannot clear it"},
		{http.StatusRequestEntityTooLarge, "invalid_request_error",
			"an oversized request must be shrunk, not retried"},
		// Already correct — these must not regress.
		{http.StatusUnauthorized, "authentication_error",
			"a revoked credential must surface as auth, not as a transient server fault"},
		{http.StatusTooManyRequests, "rate_limit_error",
			"a rate limit must be distinguishable so the client backs off"},
		// Genuine server faults stay server faults.
		{http.StatusInternalServerError, "server_error", "a real upstream failure"},
		{http.StatusBadGateway, "server_error", "a real upstream failure"},
		{http.StatusServiceUnavailable, "server_error", "a real upstream failure"},
	}
	for _, c := range cases {
		if got := errorTypeForOpenAIStatus(c.status); got != c.want {
			t.Errorf("errorTypeForOpenAIStatus(%d) = %q, want %q — %s",
				c.status, got, c.want, c.why)
		}
	}
}

// End to end through the real classifier: the two statuses the proxy actually
// produces for client-side conditions must not reach the client as server_error.
func TestOpenAIErrorTypeFromRealUpstreamErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "input too long is a client fault",
			err:  errors.New("HTTP 400 from kiro: input is too long for requested model"),
			want: "invalid_request_error",
		},
		{
			name: "overage is a client fault",
			err:  errors.New("HTTP 402 from kiro: overage limit reached"),
			want: "invalid_request_error",
		},
		{
			name: "quota stays a rate limit",
			err:  errors.New("HTTP 429 from kiro: quota exhausted"),
			want: "rate_limit_error",
		},
		{
			name: "revoked credential stays auth",
			err:  errors.New("refresh failed: 401 {\"error\":\"invalid_grant\"}"),
			want: "authentication_error",
		},
		{
			name: "upstream outage stays a server error",
			err:  errors.New("HTTP 500 from kiro: internal failure"),
			want: "server_error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := errorTypeForOpenAIStatus(statusForUpstreamError(c.err))
			if got != c.want {
				t.Fatalf("errorTypeForOpenAIStatus(statusForUpstreamError(%v)) = %q, want %q",
					c.err, got, c.want)
			}
		})
	}
}

// The two surfaces must agree on the CLASS of fault for the same upstream error.
// They use different vendor vocabularies, so the strings differ by design; what
// must not differ is whether a condition is the client's fault or the server's.
// Disagreeing there means the same failure produces opposite client behaviour
// depending only on which endpoint was called.
func TestClaudeAndOpenAISurfacesAgreeOnFaultClass(t *testing.T) {
	clientFaultClaude := map[string]bool{
		"invalid_request_error": true,
		"authentication_error":  true,
		"billing_error":         true,
		"permission_error":      true,
		"not_found_error":       true,
		"request_too_large":     true,
		"rate_limit_error":      true,
	}
	clientFaultOpenAI := map[string]bool{
		"invalid_request_error": true,
		"authentication_error":  true,
		"rate_limit_error":      true,
	}

	// Every status statusForUpstreamError can actually return.
	for _, status := range []int{400, 401, 402, 429, 500} {
		claudeType := claudeErrorTypeForStatus(status)
		openaiType := errorTypeForOpenAIStatus(status)
		claudeIsClient := clientFaultClaude[claudeType]
		openaiIsClient := clientFaultOpenAI[openaiType]
		if claudeIsClient != openaiIsClient {
			t.Errorf("status %d: fault class disagrees across surfaces — claude=%q (client=%v) vs openai=%q (client=%v); "+
				"the same upstream failure would make a client retry on one endpoint and give up on the other",
				status, claudeType, claudeIsClient, openaiType, openaiIsClient)
		}
	}
}
