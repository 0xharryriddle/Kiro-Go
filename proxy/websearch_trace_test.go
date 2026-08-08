package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// Tests for web-search trace emission (PROPOSAL D1b).
//
// Before this, websearch.go:700 and websearch_loop.go:223 called recordSuccessLog
// directly, so both web-search surfaces produced the LEGACY thin row: no
// RequestID (unjoinable), no Attempts, no outcome/status. The loop case was the
// worst of the set, because a single request can span several upstream rounds on
// DIFFERENT accounts and none of that history was recorded anywhere.
//
// The shape chosen (user decision): ONE row per request, with every round's
// account attempts accumulated on one recorder. So the assertions below come in
// pairs — the row must be rich, and there must be exactly ONE of it.
//
// These tests drive the REAL entrypoints (runWebSearchLoop /
// handleWebSearchRequest) rather than the recorder, because rounds 18f and 18g
// both shipped a false green from helper-level tests: reverting a call site to
// the legacy writer survived the entire suite. See CHECKPOINT 11c and 12d.

// stubMcpSearch points the MCP client at a local server returning one usable
// result, and restores the override on cleanup.
func stubMcpSearch(t *testing.T) {
	t.Helper()
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req McpRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      req.ID,
			"jsonrpc": "2.0",
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{
					"type": "text",
					"text": `{"results":[{"title":"t","url":"https://example.com","snippet":"s"}]}`,
				}},
			},
		})
	}))
	old := mcpEndpointOverride
	mcpEndpointOverride = mcp.URL
	t.Cleanup(func() {
		mcpEndpointOverride = old
		mcp.Close()
	})
}

// webSearchTraceEnv sets up config, N Kiro accounts, a pinned endpoint, and a
// handler with the live log ring.
func webSearchTraceEnv(t *testing.T, accountIDs ...string) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, id := range accountIDs {
		if err := config.AddAccount(config.Account{
			ID:          id,
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("AddAccount %s: %v", id, err)
		}
	}
	// swapKiroEndpointsForTest installs a ONE-element endpoint list while the
	// default "auto" preference indexes kiroEndpoints[0..2], so pin one endpoint
	// with no fallback or getSortedEndpoints panics (index out of range).
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	return &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
}

// ---------------------------------------------------------------------------
// The mixed-tools loop: one row for a multi-round, multi-account request
// ---------------------------------------------------------------------------

// THE central D1b assertion. A loop spanning two upstream rounds must emit ONE
// rich row whose Attempts carry every account tried — not one row per round
// (which would break the one-request-one-row invariant D1/D1d exist to hold),
// and not a legacy thin row with no RequestID.
func TestWebSearchLoopEmitsOneRichRowAcrossRounds(t *testing.T) {
	h := webSearchTraceEnv(t, "trace-A", "trace-B")
	stubMcpSearch(t)

	var mu sync.Mutex
	round := 0
	var servedBy []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID := strings.TrimPrefix(
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "token-")
		mu.Lock()
		round++
		n := round
		servedBy = append(servedBy, accountID)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// Round 1 asks for a web_search, so the loop continues.
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "toolu_round1",
				"name":      webSearchToolName,
				"input":     map[string]interface{}{"query": "golang"},
				"stop":      true,
			}))
			return
		}
		// Round 2 returns text, so the loop flushes.
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "final answer",
		}))
	}))
	defer srv.Close()
	defer swapKiroEndpointsForTest(t, srv)()

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search then answer"}},
		Tools: []ClaudeTool{{
			Type:    "web_search_20250305",
			Name:    webSearchToolName,
			MaxUses: 2,
		}},
	}

	rec := httptest.NewRecorder()
	h.runWebSearchLoop(rec, req, false, 10, "key-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("loop returned HTTP %d, body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	rounds := round
	served := append([]string(nil), servedBy...)
	mu.Unlock()
	// Guard against a vacuous pass: the multi-account history only exists to be
	// lost if the request really spanned more than one round.
	if rounds < 2 {
		t.Fatalf("test did not exercise a multi-round loop (rounds=%d)", rounds)
	}

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row for one request, got %d — a recorder per "+
			"round would emit one row per round and break the one-request-one-row "+
			"invariant", len(logs))
	}
	got := logs[0]
	if got.RequestID == "" {
		t.Fatal("row has no RequestID: runWebSearchLoop is still calling the legacy " +
			"recordSuccessLog instead of emitTrace")
	}
	if got.Outcome != outcomeSuccess {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, outcomeSuccess)
	}
	// The whole point of the chosen shape: attempts span the rounds, so the
	// upstream calls plus the MCP search between them are all on one row.
	if got.AttemptCount < rounds {
		t.Fatalf("AttemptCount = %d but the request made %d upstream rounds; the "+
			"per-round attempts are being dropped instead of accumulating on one "+
			"recorder (attempts=%+v, servedBy=%v)",
			got.AttemptCount, rounds, got.Attempts, served)
	}
	t.Logf("rounds=%d servedBy=%v attempts=%d requestID=%s",
		rounds, served, got.AttemptCount, got.RequestID)
}

// A round that fails over must keep the FAILED attempt, with its cause and the
// account it belongs to, on the same row as the successful one.
//
// This is the failover evidence the whole trace subsystem exists to carry, and
// the mutation battery proved the count-only assertions above do not constrain
// it: two mutants survived — beginAttempt(nil) (attempts lose account identity)
// and endAttempt(att, nil) on the failure branch (a failed round recorded as a
// success). Both leave AttemptCount correct, so only per-attempt identity and
// outcome assertions can kill them.
func TestWebSearchLoopRecordsFailedRoundAttemptWithItsCause(t *testing.T) {
	h := webSearchTraceEnv(t, "fail-A", "fail-B")

	// The FIRST upstream call fails; the retry inside callUpstreamForWebSearch
	// then succeeds on another account. Keyed on call order rather than account
	// ID because selection is quota/health-aware LRU, so which account is tried
	// first is not ours to assume.
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"upstream exploded"}`))
			return
		}
		// Plain text and no tool_use, so this round is terminal and the loop
		// flushes without needing MCP.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "answer after failover",
		}))
	}))
	defer srv.Close()
	defer swapKiroEndpointsForTest(t, srv)()

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search then answer"}},
		Tools: []ClaudeTool{{
			Type:    "web_search_20250305",
			Name:    webSearchToolName,
			MaxUses: 2,
		}},
	}

	rec := httptest.NewRecorder()
	h.runWebSearchLoop(rec, req, false, 10, "key-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("the retry should have served this request, got HTTP %d: %s",
			rec.Code, rec.Body.String())
	}

	mu.Lock()
	upstreamCalls := calls
	mu.Unlock()
	// Guard against a vacuous pass: without a real failover there is no failed
	// attempt to lose, and both mutants would look fine.
	if upstreamCalls < 2 {
		t.Fatalf("no failover happened (upstream calls=%d); this test cannot see "+
			"the defect it exists for", upstreamCalls)
	}

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(logs))
	}
	got := logs[0]
	if got.AttemptCount < 2 {
		t.Fatalf("AttemptCount = %d, want >= 2: the failed attempt was dropped, so "+
			"the row cannot show why the request rerouted (attempts=%+v)",
			got.AttemptCount, got.Attempts)
	}

	// Kills "attempts lose account identity": an attempt that does not name its
	// account is useless for deciding which credential misbehaved, and it also
	// empties the request-level AccountID emitTrace derives from the last attempt.
	for i, att := range got.Attempts {
		if att.AccountID == "" {
			t.Fatalf("attempt %d has no AccountID, so the row cannot say which "+
				"account it describes: %+v", i+1, att)
		}
	}
	if got.AccountID == "" {
		t.Fatal("request-level AccountID is empty; it is derived from the last " +
			"attempt, so the attempts are not carrying their account")
	}

	// Kills "failed round attempts recorded as successes": the failure must be
	// recorded AS a failure, with its cause preserved.
	var failed int
	for _, att := range got.Attempts {
		if att.Outcome == outcomeError {
			failed++
			if att.Error == "" {
				t.Errorf("failed attempt on %s recorded no cause: %+v",
					att.AccountID, att)
			}
		}
	}
	if failed == 0 {
		t.Fatalf("no attempt is marked as failed even though %d upstream calls were "+
			"made and the first one returned HTTP 500; a reroute is being recorded "+
			"as a clean success (attempts=%+v)", upstreamCalls, got.Attempts)
	}

	// The request as a whole SUCCEEDED — the failover worked. A row that reports
	// the request itself as an error would be the opposite defect.
	if got.Outcome != outcomeSuccess {
		t.Fatalf("Outcome = %q, want %q: the retry served the request", got.Outcome, outcomeSuccess)
	}
	t.Logf("upstreamCalls=%d attempts=%d failed=%d accountID=%s",
		upstreamCalls, got.AttemptCount, failed, got.AccountID)
}

// A round failure must emit ONE error row and move the counters exactly once.
// emitTrace(outcomeError) counts by itself, so pairing it with the old
// recordFailureWithDetails would log twice and double-count (defect 81).
func TestWebSearchLoopRoundFailureCountsOnce(t *testing.T) {
	h := webSearchTraceEnv(t, "trace-A")

	// Every upstream round fails, so the loop exhausts its retries and reports.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()
	defer swapKiroEndpointsForTest(t, srv)()

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search then answer"}},
		Tools: []ClaudeTool{{
			Type:    "web_search_20250305",
			Name:    webSearchToolName,
			MaxUses: 2,
		}},
	}

	rec := httptest.NewRecorder()
	h.runWebSearchLoop(rec, req, false, 10, "key-1")

	if rec.Code == http.StatusOK {
		t.Fatalf("expected an error response, got 200: %s", rec.Body.String())
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want exactly 1 — emitTrace and a counting "+
			"failure recorder must not both count the same failure", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests = %d, want exactly 1", got)
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(logs))
	}
	if logs[0].RequestID == "" {
		t.Fatal("failure row has no RequestID: still the legacy flat writer")
	}
	if logs[0].Outcome != outcomeError {
		t.Fatalf("Outcome = %q, want %q", logs[0].Outcome, outcomeError)
	}
}

// ---------------------------------------------------------------------------
// The pure web_search surface
// ---------------------------------------------------------------------------

// handleWebSearchRequest is dispatched from handler.go BEFORE that function's
// first newTraceRecorder, so unlike the passthrough paths of D1/D1d it has to
// CREATE the recorder. This pins that it does, and that only one row results.
func TestPureWebSearchEmitsOneRichRow(t *testing.T) {
	h := webSearchTraceEnv(t, "trace-A")
	stubMcpSearch(t)

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search for golang"}},
		Tools: []ClaudeTool{{
			Type: "web_search_20250305",
			Name: webSearchToolName,
		}},
	}

	rec := httptest.NewRecorder()
	h.handleWebSearchRequest(rec, req, 10, "key-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("pure web search returned HTTP %d, body=%s", rec.Code, rec.Body.String())
	}

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(logs))
	}
	got := logs[0]
	if got.RequestID == "" {
		t.Fatal("row has no RequestID: handleWebSearchRequest is still calling the " +
			"legacy recordSuccessLog")
	}
	if got.Outcome != outcomeSuccess {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, outcomeSuccess)
	}
	// performWebSearch opens one attempt per account it tries, so the serving
	// account must appear rather than the row being attempt-less.
	if got.AttemptCount < 1 {
		t.Fatalf("AttemptCount = %d, want >= 1: performWebSearch is not recording "+
			"the account it used", got.AttemptCount)
	}
	if got.AccountID != "trace-A" {
		t.Fatalf("AccountID = %q, want trace-A (derived from the last attempt)", got.AccountID)
	}
}

// The failure half of the pure surface: one error row, counted once.
func TestPureWebSearchFailureCountsOnce(t *testing.T) {
	h := webSearchTraceEnv(t, "trace-A")

	// MCP fails outright, so performWebSearch exhausts every account.
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"mcp exploded"}`))
	}))
	defer mcp.Close()
	old := mcpEndpointOverride
	mcpEndpointOverride = mcp.URL
	defer func() { mcpEndpointOverride = old }()

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search for golang"}},
		Tools: []ClaudeTool{{
			Type: "web_search_20250305",
			Name: webSearchToolName,
		}},
	}

	rec := httptest.NewRecorder()
	h.handleWebSearchRequest(rec, req, 10, "key-1")

	if rec.Code == http.StatusOK {
		t.Fatalf("expected an error response, got 200: %s", rec.Body.String())
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want exactly 1", got)
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(logs))
	}
	if logs[0].Outcome != outcomeError {
		t.Fatalf("Outcome = %q, want %q", logs[0].Outcome, outcomeError)
	}
	// Attempts must carry the cause, or the row cannot say why the search failed.
	if logs[0].AttemptCount < 1 || logs[0].Attempts[0].Error == "" {
		t.Fatalf("attempt lost its failure cause: %+v", logs[0].Attempts)
	}
}
