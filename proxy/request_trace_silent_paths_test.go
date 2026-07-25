package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A response served from the in-process cache still consumes the tenant's quota,
// so it must appear in the request logs. Before this, the cache-hit path bumped
// the global counters without ever emitting a RequestLog, which made logCount
// and totalRequests permanently irreconcilable.
func TestCacheHitIsLogged(t *testing.T) {
	h := &Handler{}
	tr := newTraceRecorder("claude", "sonnet", false, "key-1")
	tr.noteUsage(11, 7, 0, 0)
	tr.markCacheHit()
	h.emitTrace(tr, outcomeCacheHit, http.StatusOK)

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly one record for a cache hit, got %d", len(logs))
	}
	got := logs[0]
	if got.Outcome != outcomeCacheHit {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, outcomeCacheHit)
	}
	if !got.CacheHit {
		t.Fatal("CacheHit must be set so the row is distinguishable from an upstream call")
	}
	// Status stays "success" because account health and usage-anomaly
	// aggregation switch on it.
	if got.Status != "success" {
		t.Fatalf("Status = %q, want success", got.Status)
	}
	if got.Tokens != 18 {
		t.Fatalf("Tokens = %d, want 18 (quota must still be attributed)", got.Tokens)
	}
	// A cache hit never touches upstream, so it must not fabricate an attempt.
	if got.AttemptCount != 0 || len(got.Attempts) != 0 {
		t.Fatalf("cache hit must record no upstream attempts, got %d", got.AttemptCount)
	}
}

// Requests rejected before reaching a handler (bad key, rate limit) produced
// zero trace evidence, so a tenant hammering with an invalid key was invisible.
func TestRejectedRequestsAreLogged(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		status    int
		errorType string
	}{
		{"unauthorized", "invalid_api_key", http.StatusUnauthorized, "auth"},
		{"rate limited", "rpm", http.StatusTooManyRequests, "rate_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			h.recordRejection("claude", "key-1", tc.reason, tc.status)

			logs := h.getRequestLogs()
			if len(logs) != 1 {
				t.Fatalf("expected one record, got %d", len(logs))
			}
			got := logs[0]
			if got.Outcome != outcomeRejected {
				t.Fatalf("Outcome = %q, want %q", got.Outcome, outcomeRejected)
			}
			if got.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, tc.status)
			}
			if got.ErrorType != tc.errorType {
				t.Fatalf("ErrorType = %q, want %q", got.ErrorType, tc.errorType)
			}
			if got.Status != "error" {
				t.Fatalf("Status = %q, want error", got.Status)
			}
			if !strings.HasPrefix(got.RequestID, "trc_") {
				t.Fatalf("rejection must still be joinable, RequestID = %q", got.RequestID)
			}
		})
	}
}

// A rejection record must never carry the offending credential, even though the
// rejection is *about* that credential.
func TestRejectionNeverLogsKeyMaterial(t *testing.T) {
	h := &Handler{}
	h.recordRejection("claude", "ksk_live_supersecret", "invalid_api_key", http.StatusUnauthorized)

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected one record, got %d", len(logs))
	}
	raw, err := json.Marshal(logs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "ksk_live_supersecret") {
		t.Fatalf("rejection record leaked key material: %s", raw)
	}
}

// Clearing the logs destroys evidence, so it must leave an audit trail.
func TestClearLogsIsAudited(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{{Time: 1, Endpoint: "claude", Status: "success"}}

	h.auditLogClear(1)

	h.auditLogsMu.RLock()
	defer h.auditLogsMu.RUnlock()
	if len(h.auditLogs) != 1 {
		t.Fatalf("expected one audit entry, got %d", len(h.auditLogs))
	}
	entry := h.auditLogs[0]
	if entry.Category != "logs" || entry.Action != "clear" {
		t.Fatalf("unexpected audit entry: %#v", entry)
	}
	if entry.SafeDetails["clearedCount"] != "1" {
		t.Fatalf("clearedCount = %q, want 1", entry.SafeDetails["clearedCount"])
	}
}
