package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// This test exists because a helper-level test was NOT enough.
//
// The unit tests in request_trace_counters_test.go call recordFailureAttribution
// and emitTrace directly, so restoring the original bug AT THE CALL SITE
// (responses_handler.go:752 calling the counting variant again) left the whole
// suite green — a false green for the very fix it was meant to protect. Verified
// by mutation: that call-site mutant SURVIVED until this test existed.
//
// So this drives the real handler and counts what the real path produces.
//
// The defect: /v1/responses streaming called emitTrace(outcomeError) and then
// recordFailureForApiKey. Both bump totalRequests and failedRequests
// (request_trace_recorder.go:362-365 and handler.go recordFailure), so ONE failed
// request incremented both counters TWICE. handler.go's own note above
// recordSuccessLog warns about exactly this: "Do not reintroduce them on a route
// that already emits a trace: that route would then log twice and double-count
// totalRequests."
func TestResponsesStreamCountsMidStreamFailureOnce(t *testing.T) {
	h := setupMidStreamFailureHandler(t, midStreamLongText)

	before := atomic.LoadInt64(&h.failedRequests)
	beforeTotal := atomic.LoadInt64(&h.totalRequests)

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, midStreamPayload(), "claude-opus-4.6", false, 5, "key-1",
		"resp_test", &ResponsesRequest{Model: "claude-opus-4.6"}, json.RawMessage(`"hi"`), false,
		nil, false)

	// Asserted, not skipped: the fake upstream deterministically writes one valid
	// frame and then a truncated one, so the mid-stream failure path is always
	// taken. Skipping here would make the whole test vacuous — which is how the
	// call-site mutant survived in the first place.
	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("expected the mid-stream failure path (response.failed) to be exercised; "+
			"without it this test proves nothing\nbody:\n%s", body)
	}

	if got := atomic.LoadInt64(&h.failedRequests) - before; got != 1 {
		t.Fatalf("failedRequests advanced by %d, want exactly 1 — a single failed request "+
			"must not be counted by both emitTrace and the failure recorder", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests) - beforeTotal; got != 1 {
		t.Fatalf("totalRequests advanced by %d, want exactly 1", got)
	}
}

// Control: the failure must still reach the trace store as ONE row, so fixing the
// double-count cannot be satisfied by dropping the row instead.
func TestResponsesStreamMidStreamFailureEmitsOneTraceRow(t *testing.T) {
	h := setupMidStreamFailureHandler(t, midStreamLongText)

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, midStreamPayload(), "claude-opus-4.6", false, 5, "key-1",
		"resp_test", &ResponsesRequest{Model: "claude-opus-4.6"}, json.RawMessage(`"hi"`), false,
		nil, false)

	if !strings.Contains(rec.Body.String(), "response.failed") {
		t.Fatal("mid-stream failure path was not exercised")
	}

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 trace row for the failed request, got %d", len(logs))
	}
	if logs[0].Outcome != outcomeError {
		t.Fatalf("Outcome = %q, want %q", logs[0].Outcome, outcomeError)
	}
	if logs[0].RequestID == "" {
		t.Fatal("failed request row has no RequestID, so it cannot be joined")
	}
}
