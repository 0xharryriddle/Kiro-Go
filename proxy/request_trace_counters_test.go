package proxy

import (
	"net/http"
	"sync/atomic"
	"testing"
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
