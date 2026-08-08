package proxy

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Failure counting used to live inside recordFailureWithDetails. When the trace
// recorder replaced that function at every call site, the counter increment had
// to move with it — otherwise failedRequests silently stops advancing and the
// dashboard reports a 100% success rate while requests are failing.
func TestEmitTraceCountsFailures(t *testing.T) {
	h := &Handler{}
	tr := newTraceRecorder("claude", "sonnet", false, "")
	h.emitTrace(tr, outcomeError, http.StatusInternalServerError)

	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests = %d, want 1", got)
	}
}

// Success counting stays with recordSuccessForApiKey, which every success path
// already calls. emitTrace must NOT double-count it.
func TestEmitTraceDoesNotDoubleCountSuccess(t *testing.T) {
	h := &Handler{}
	h.recordSuccess(1, 1, 0) // what the serving path already does
	tr := newTraceRecorder("claude", "sonnet", false, "")
	h.emitTrace(tr, outcomeSuccess, http.StatusOK)

	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests = %d, want 1 (emitTrace must not re-count)", got)
	}
	if got := atomic.LoadInt64(&h.successRequests); got != 1 {
		t.Fatalf("successRequests = %d, want 1", got)
	}
}

// A route that emits a trace AND attributes the failure to a key must count the
// failed request exactly ONCE. `/v1/responses` streaming called emitTrace(
// outcomeError) followed by recordFailureForApiKey (responses_handler.go:752-753)
// and both bump totalRequests/failedRequests, so one failure was counted twice —
// the exact double-count handler.go's own note warns against. The attribution-only
// variant exists so both can run on one path.
func TestFailedRequestIsCountedExactlyOnceWhenTraced(t *testing.T) {
	h := &Handler{}
	tr := newTraceRecorder("openai", "gpt-x", true, "key-1")

	// What the responses streaming failure path does.
	h.emitTrace(tr, outcomeError, http.StatusBadGateway)
	h.recordFailureAttribution("key-1", "openai", "gpt-x", 0, "upstream exploded", time.Time{})

	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want 1 — one failed request must not be counted twice", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests = %d, want 1", got)
	}
}

// The attribution-only variant must not touch the global counters at all;
// otherwise it cannot be paired with emitTrace.
func TestRecordFailureAttributionDoesNotCount(t *testing.T) {
	h := &Handler{}
	h.recordFailureAttribution("key-1", "claude", "sonnet", 500, "boom", time.Time{})

	if got := atomic.LoadInt64(&h.totalRequests); got != 0 {
		t.Fatalf("totalRequests = %d, want 0 (attribution must not count)", got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 0 {
		t.Fatalf("failedRequests = %d, want 0 (attribution must not count)", got)
	}
}

// Control: the COUNTING variant must keep counting, or every untraced failure
// path (Claude/OpenAI tails, websearch) silently stops advancing the counters.
func TestRecordFailureForApiKeyStillCounts(t *testing.T) {
	h := &Handler{}
	h.recordFailureForApiKey("key-1", "claude", "sonnet", 500, "boom", time.Time{})

	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests = %d, want 1", got)
	}
}

// Rejections never reached an upstream account. They were not counted before
// this change and must stay out of the global request counters, so that
// totalRequests keeps meaning "requests we actually attempted to serve".
func TestRejectionsStayOutOfGlobalCounters(t *testing.T) {
	h := &Handler{}
	h.recordRejection("claude", "key-1", "invalid_api_key", http.StatusUnauthorized)

	if got := atomic.LoadInt64(&h.totalRequests); got != 0 {
		t.Fatalf("totalRequests = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 0 {
		t.Fatalf("failedRequests = %d, want 0", got)
	}
	// ...but it must still be visible in the logs.
	if logs := h.getRequestLogs(); len(logs) != 1 {
		t.Fatalf("expected the rejection to be logged, got %d records", len(logs))
	}
}
