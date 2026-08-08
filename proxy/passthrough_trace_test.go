package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// Tests for passthrough trace emission (passthrough_trace.go / PROPOSAL D1).
//
// The defect: Bedrock and custom_api requests DID produce a request-log row (via
// recordSuccessLog), but the legacy minimal one — no RequestID, no Outcome, no
// attempts, no token split, no cache figures. Worse, a traceRecorder had already
// been allocated and an attempt already opened against the serving account, then
// the passthrough branch returned without closing either, so a failover chain
// ending on a passthrough account discarded the history of every account that
// failed before it.

// passthroughHandler builds a Handler with the live log ring and a real pool,
// which recordBedrockSuccessWithCache needs (it calls pool.RecordSuccess).
func passthroughHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	t.Cleanup(h.pool.WaitForPendingWrites)
	return h
}

func tracedParams(tr *traceRecorder, att *traceAttempt, acc *config.Account) forwardParams {
	return forwardParams{
		account: acc, model: "claude-sonnet-4.5", endpoint: "anthropic",
		apiKeyID: "key-1", trace: tr, attempt: att,
	}
}

// ---------------------------------------------------------------------------
// The row-count invariant: exactly one, never two
// ---------------------------------------------------------------------------

// THE central regression guard. emitTrace and recordSuccessLog both append to the
// same store, so wiring the rich trace in without removing the legacy call would
// write TWO rows per request — the exact row-inflation emitTrace's doc comment
// says it exists to prevent.
func TestPassthroughEmitsExactlyOneRow(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock", Email: "a@example.com"}
	tr := newTraceRecorder("claude", "claude-sonnet-4.5", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordBedrockSuccessWithCache(tracedParams(tr, att, acc), 100, 20, 0, 0, time.Now())

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 request-log row, got %d — two rows means the "+
			"legacy recordSuccessLog and emitTrace both fired for one request", len(logs))
	}
}

// The other half of the invariant: it must not be ZERO either. A refactor that
// dropped the emit entirely would satisfy "not two" while making every
// passthrough request invisible.
func TestPassthroughWithoutTraceStillLogs(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock"}

	// No recorder threaded (bedrockTestReply, admin probes, older tests).
	h.recordBedrockSuccessWithCache(forwardParams{
		account: acc, model: "m", endpoint: "anthropic", apiKeyID: "key-1",
	}, 10, 5, 0, 0, time.Now())

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("an untraced passthrough must still produce its legacy row, got %d", len(logs))
	}
	if logs[0].Tokens != 15 {
		t.Fatalf("legacy row lost its token total: got %d, want 15", logs[0].Tokens)
	}
}

// ---------------------------------------------------------------------------
// The row is RICH, not the legacy minimal shape
// ---------------------------------------------------------------------------

func TestPassthroughRowCarriesTraceFields(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock", Email: "a@example.com"}
	tr := newTraceRecorder("claude", "claude-sonnet-4.5", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordBedrockSuccessWithCache(tracedParams(tr, att, acc), 100, 20, 70, 5, time.Now())

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(logs))
	}
	got := logs[0]

	// RequestID is what makes the row joinable at all; the legacy row had none.
	if got.RequestID == "" {
		t.Fatal("row has no RequestID, so it cannot be joined to anything")
	}
	if got.RequestID != tr.TraceID() {
		t.Fatalf("RequestID = %q, want the recorder's trace id %q", got.RequestID, tr.TraceID())
	}
	if got.Outcome != outcomeSuccess {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, outcomeSuccess)
	}
	if got.Status != "success" {
		t.Fatalf("Status = %q, want success (account-health aggregation switches on it)", got.Status)
	}
	if got.API != "claude" {
		t.Fatalf("API = %q, want claude", got.API)
	}
	if !got.Stream {
		t.Fatal("Stream must be set for a streaming passthrough")
	}
	if got.HTTPStatus != 200 {
		t.Fatalf("HTTPStatus = %d, want 200", got.HTTPStatus)
	}
	// The token SPLIT is the point: the legacy row carried only the sum.
	if got.InputTokens != 100 || got.OutputTokens != 20 {
		t.Fatalf("token split = (%d,%d), want (100,20)", got.InputTokens, got.OutputTokens)
	}
	if got.Tokens != 120 {
		t.Fatalf("Tokens = %d, want 120 (sum stays for backward compatibility)", got.Tokens)
	}
	if got.AccountID != "acct-1" {
		t.Fatalf("AccountID = %q, want acct-1", got.AccountID)
	}
	if got.AccountEmail != "a@example.com" {
		t.Fatalf("AccountEmail = %q, want a@example.com", got.AccountEmail)
	}
}

// Cache figures must survive to the row. Without this the trace cannot
// distinguish "large input, served from cache" from "context went missing" —
// two facts with opposite remedies.
func TestPassthroughRowCarriesCacheBreakdown(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock"}
	tr := newTraceRecorder("claude", "m", false, "key-1")
	att := tr.beginAttempt(acc)

	h.recordBedrockSuccessWithCache(tracedParams(tr, att, acc), 350, 20, 200, 100, time.Now())

	got := h.getRequestLogs()[0]
	if got.CacheReadTokens != 200 {
		t.Fatalf("CacheReadTokens = %d, want 200", got.CacheReadTokens)
	}
	if got.CacheWriteTokens != 100 {
		t.Fatalf("CacheWriteTokens = %d, want 100", got.CacheWriteTokens)
	}
	// Cache tokens are a BREAKDOWN of input, never an addition: billing must not
	// double-count them.
	if got.Tokens != 370 {
		t.Fatalf("Tokens = %d, want 370 (350 input + 20 output); cache tokens must "+
			"not be added on top of an input figure that already contains them", got.Tokens)
	}
}

// ---------------------------------------------------------------------------
// Failover history: the evidence that was being discarded
// ---------------------------------------------------------------------------

// A chain that fails on one account and succeeds on a passthrough account must
// produce ONE row carrying BOTH attempts. Before this, the successful passthrough
// returned without touching the recorder, so every earlier failure vanished.
func TestPassthroughPreservesFailoverAttempts(t *testing.T) {
	h := passthroughHandler(t)
	tr := newTraceRecorder("claude", "m", false, "key-1")

	// Attempt 1: a Kiro account that failed and was excluded.
	bad := &config.Account{ID: "acct-bad", Email: "bad@example.com"}
	badAtt := tr.beginAttempt(bad)
	h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: badAtt},
		errors.New("upstream status 429: ThrottlingException"))

	// Attempt 2: the Bedrock account that served it.
	good := &config.Account{ID: "acct-good", AuthMethod: "bedrock", Email: "good@example.com"}
	goodAtt := tr.beginAttempt(good)
	h.recordBedrockSuccessWithCache(tracedParams(tr, goodAtt, good), 10, 5, 0, 0, time.Now())

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("a failover chain must still yield ONE row, got %d", len(logs))
	}
	got := logs[0]
	if got.AttemptCount != 2 {
		t.Fatalf("AttemptCount = %d, want 2 — the failed account's attempt was discarded, "+
			"which is exactly the evidence an operator needs", got.AttemptCount)
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("len(Attempts) = %d, want 2", len(got.Attempts))
	}
	if got.Attempts[0].AccountID != "acct-bad" || got.Attempts[0].Outcome != outcomeError {
		t.Fatalf("first attempt should be the failed account, got %+v", got.Attempts[0])
	}
	if got.Attempts[0].Error == "" {
		t.Fatal("the failed attempt lost its cause")
	}
	if got.Attempts[1].AccountID != "acct-good" || got.Attempts[1].Outcome != outcomeSuccess {
		t.Fatalf("second attempt should be the serving account, got %+v", got.Attempts[1])
	}
	// Request-level routing context comes from the attempt that served it.
	if got.AccountID != "acct-good" {
		t.Fatalf("AccountID = %q, want the SERVING account acct-good", got.AccountID)
	}
}

// A failed attempt on its own must NOT emit a row: the request is not over, it is
// being retried elsewhere. Emitting here would produce one row per attempt.
func TestFailedPassthroughAttemptEmitsNoRow(t *testing.T) {
	h := passthroughHandler(t)
	tr := newTraceRecorder("claude", "m", false, "key-1")
	acc := &config.Account{ID: "acct-bad"}
	att := tr.beginAttempt(acc)

	h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: att}, errors.New("boom"))

	if logs := h.getRequestLogs(); len(logs) != 0 {
		t.Fatalf("a failed attempt must not emit a row (the request is being retried), got %d", len(logs))
	}
}

// Control: a nil error must not fabricate a failed attempt.
func TestFailedPassthroughAttemptIgnoresNilError(t *testing.T) {
	h := passthroughHandler(t)
	tr := newTraceRecorder("claude", "m", false, "key-1")
	att := tr.beginAttempt(&config.Account{ID: "acct"})

	h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: att}, nil)

	entry := tr.finish(outcomeSuccess, 200)
	if entry.AttemptCount != 0 {
		t.Fatalf("a nil error must not close an attempt, got AttemptCount=%d", entry.AttemptCount)
	}
}

// ---------------------------------------------------------------------------
// Partial failure: the stream broke AFTER the client got bytes (D1d)
// ---------------------------------------------------------------------------

// A traced partial failure must produce ONE rich row: the client already holds a
// partial body, so the request is over and exactly one terminal row is owed.
func TestPassthroughPartialFailureEmitsRichRow(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock", Email: "a@example.com"}
	tr := newTraceRecorder("claude", "claude-sonnet-4.5", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordPassthroughPartialFailure(tracedParams(tr, att, acc), "claude",
		errors.New("bedrock stream: ThrottlingException: slow down"))

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 terminal row for a partial failure, got %d", len(logs))
	}
	got := logs[0]
	if got.RequestID == "" {
		t.Fatal("partial-failure row has no RequestID, so it cannot be joined")
	}
	if got.Outcome != outcomeError {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, outcomeError)
	}
	if got.Status != "error" {
		t.Fatalf("Status = %q, want error", got.Status)
	}
	// The attempt must carry the cause, or the row cannot say WHICH account broke.
	if got.AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want 1", got.AttemptCount)
	}
	if got.Attempts[0].Outcome != outcomeError || got.Attempts[0].Error == "" {
		t.Fatalf("attempt lost its failure cause: %+v", got.Attempts[0])
	}
	if got.AccountID != "acct-1" {
		t.Fatalf("AccountID = %q, want acct-1", got.AccountID)
	}
}

// THE D1d counter invariant, and the reason this fix needed the same helper split
// as defect 81: emitTrace(outcomeError) counts, so pairing it with the COUNTING
// failure recorder would advance the counters by two for one failed request.
func TestPassthroughPartialFailureCountsExactlyOnce(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock"}
	tr := newTraceRecorder("claude", "m", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordPassthroughPartialFailure(tracedParams(tr, att, acc), "claude", errors.New("boom"))

	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want exactly 1 — emitTrace and the failure "+
			"recorder must not both count the same failed request", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests = %d, want exactly 1", got)
	}
}

// Control: the untraced path must keep its pre-D1d behaviour — one legacy row AND
// one counter bump. Without this, moving the count into emitTrace would silently
// stop counting partial failures on untraced callers.
func TestPassthroughPartialFailureWithoutTraceStillCountsAndLogs(t *testing.T) {
	h := passthroughHandler(t)

	h.recordPassthroughPartialFailure(forwardParams{
		account: &config.Account{ID: "acct-1"}, model: "m", endpoint: "anthropic",
	}, "claude", errors.New("boom"))

	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want 1 for an untraced partial failure", got)
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected the legacy row, got %d", len(logs))
	}
	if logs[0].Status != "error" {
		t.Fatalf("Status = %q, want error", logs[0].Status)
	}
}

// The status reported must be 200: the headers were already written and flushed
// with 200 before the stream broke, so 200 is what the client actually received.
// Reporting 502 would describe a response nobody was sent.
func TestPassthroughPartialFailureReportsCommittedStatus(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1"}
	tr := newTraceRecorder("claude", "m", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordPassthroughPartialFailure(tracedParams(tr, att, acc), "claude", errors.New("boom"))

	if got := h.getRequestLogs()[0].HTTPStatus; got != 200 {
		t.Fatalf("HTTPStatus = %d, want 200 (headers were committed before the break)", got)
	}
}

// A nil error must not fabricate a failure row or move the counters.
func TestPassthroughPartialFailureIgnoresNilError(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1"}
	tr := newTraceRecorder("claude", "m", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordPassthroughPartialFailure(tracedParams(tr, att, acc), "claude", nil)

	if logs := h.getRequestLogs(); len(logs) != 0 {
		t.Fatalf("a nil error must not emit a row, got %d", len(logs))
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 0 {
		t.Fatalf("failedRequests = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// D1d at the CALL SITES, not the choke point
// ---------------------------------------------------------------------------
//
// The five tests above drive recordPassthroughPartialFailure directly, and a
// mutation battery proved that is NOT enough: reverting either call site to
// recordFailureWithDetails survived the whole suite. Same false green as defect 81
// (see CHECKPOINT §11c) — a helper-level test proves the helper works, not that it
// is wired in. These two drive the real callers.

// midStreamBreakBody yields one SSE chunk, then fails with a non-EOF error, which
// is the shape streamUpstream treats as "client already has bytes, cannot fail
// over".
type midStreamBreakBody struct{ sent bool }

func (b *midStreamBreakBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, []byte("data: {\"type\":\"content_block_delta\"}\n\n")), nil
	}
	return 0, errors.New("upstream connection reset by peer")
}

func (b *midStreamBreakBody) Close() error { return nil }

// Kills the mutant "bedrock call site reverted to the legacy writer".
func TestBedrockPartialFailureCallSiteEmitsRichRow(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "bedrock"}
	tr := newTraceRecorder("claude", "claude-sonnet-4.5", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordBedrockPartialFailure(tracedParams(tr, att, acc),
		errors.New("bedrock stream: ThrottlingException: slow down"))

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(logs))
	}
	if logs[0].RequestID == "" {
		t.Fatal("row has no RequestID: recordBedrockPartialFailure is still calling " +
			"the legacy flat writer instead of recordPassthroughPartialFailure")
	}
	if logs[0].AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want 1: the open attempt was discarded", logs[0].AttemptCount)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want exactly 1", got)
	}
}

// Kills the mutant "custom_api call site reverted to the legacy writer", driving
// the real streamUpstream loop through a stream that breaks after first byte.
func TestCustomApiPartialFailureCallSiteEmitsRichRow(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-1", AuthMethod: "custom_api"}
	tr := newTraceRecorder("claude", "claude-sonnet-4.5", true, "key-1")
	att := tr.beginAttempt(acc)

	rec := httptest.NewRecorder()
	err := h.streamUpstream(rec, rec,
		&http.Response{StatusCode: 200, Body: &midStreamBreakBody{}},
		tracedParams(tr, att, acc), time.Now())

	// nil is required: returning an error here would make the caller try to fail
	// over onto a response whose headers are already committed.
	if err != nil {
		t.Fatalf("streamUpstream returned %v, want nil for a post-first-byte break", err)
	}
	// Confirm we really exercised the partial branch rather than a no-output path.
	if rec.Body.Len() == 0 {
		t.Fatal("client received no bytes; this is not the partial-failure branch")
	}

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(logs))
	}
	if logs[0].RequestID == "" {
		t.Fatal("row has no RequestID: the custom_api partial-stream branch is still " +
			"calling the legacy flat writer")
	}
	if logs[0].Status != "error" {
		t.Fatalf("Status = %q, want error", logs[0].Status)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests = %d, want exactly 1", got)
	}
}

// ---------------------------------------------------------------------------
// custom_api: the sibling passthrough (option 2 — fix the class, not the site)
// ---------------------------------------------------------------------------

func TestCustomApiPassthroughEmitsRichRow(t *testing.T) {
	h := passthroughHandler(t)
	acc := &config.Account{ID: "acct-custom", AuthMethod: "custom_api", Email: "c@example.com"}
	tr := newTraceRecorder("openai", "gpt-x", true, "key-1")
	att := tr.beginAttempt(acc)

	h.recordPassthroughTrace(forwardParams{
		account: acc, model: "gpt-x", endpoint: "openai", apiKeyID: "key-1",
		trace: tr, attempt: att,
	}, "openai", passthroughUsage{inputTokens: 40, outputTokens: 8, credits: 1.5}, time.Now())

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(logs))
	}
	got := logs[0]
	if got.RequestID == "" {
		t.Fatal("custom_api row has no RequestID — the sibling passthrough shares the defect")
	}
	if got.InputTokens != 40 || got.OutputTokens != 8 {
		t.Fatalf("token split = (%d,%d), want (40,8)", got.InputTokens, got.OutputTokens)
	}
	if got.Credits != 1.5 {
		t.Fatalf("Credits = %v, want 1.5", got.Credits)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want 1", got.AttemptCount)
	}
}

// ---------------------------------------------------------------------------
// Nil-safety: every construction site that does not trace must keep working
// ---------------------------------------------------------------------------

func TestPassthroughTraceIsNilSafe(t *testing.T) {
	h := passthroughHandler(t)

	// No account, no trace, no attempt — the shape bedrockTestReply builds.
	h.recordPassthroughTrace(forwardParams{model: "m", endpoint: "anthropic"},
		"claude", passthroughUsage{inputTokens: 1, outputTokens: 1}, time.Now())

	if logs := h.getRequestLogs(); len(logs) != 1 {
		t.Fatalf("expected the legacy fallback row, got %d", len(logs))
	}
	// And the failure helper must tolerate a completely empty params struct.
	h.notePassthroughFailedAttempt(forwardParams{}, errors.New("x"))
}

// ---------------------------------------------------------------------------
// The cache extractors
// ---------------------------------------------------------------------------

func TestExtractInputCacheTokensReadsBreakdown(t *testing.T) {
	event := []byte(`{"type":"message_start","message":{"usage":{` +
		`"input_tokens":50,"cache_creation_input_tokens":100,"cache_read_input_tokens":200}}}`)

	read, write := extractInputCacheTokens(event)
	if read != 200 {
		t.Fatalf("cacheRead = %d, want 200", read)
	}
	if write != 100 {
		t.Fatalf("cacheWrite = %d, want 100", write)
	}
	// Consistency with the billing figure: total input must already include both,
	// so the row can report the breakdown without double-counting.
	if total := extractInputTokens(event); total != 350 {
		t.Fatalf("extractInputTokens = %d, want 350 (50+100+200) — the breakdown must be "+
			"a SUBSET of the billed input, not an addition to it", total)
	}
}

func TestExtractNonStreamCacheTokensReadsBreakdown(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":3,` +
		`"cache_creation_input_tokens":7,"cache_read_input_tokens":21}}`)

	read, write := extractNonStreamCacheTokens(body)
	if read != 21 || write != 7 {
		t.Fatalf("(read,write) = (%d,%d), want (21,7)", read, write)
	}
	in, out := extractNonStreamUsage(body)
	if in != 38 || out != 3 {
		t.Fatalf("usage = (%d,%d), want (38,3)", in, out)
	}
}

// Malformed input must degrade to zeros rather than panicking on the hot path.
func TestCacheExtractorsToleratesGarbage(t *testing.T) {
	for _, raw := range []string{``, `not json`, `{}`, `{"message":{}}`, `{"usage":null}`} {
		if r, w := extractInputCacheTokens([]byte(raw)); r != 0 || w != 0 {
			t.Fatalf("extractInputCacheTokens(%q) = (%d,%d), want (0,0)", raw, r, w)
		}
		if r, w := extractNonStreamCacheTokens([]byte(raw)); r != 0 || w != 0 {
			t.Fatalf("extractNonStreamCacheTokens(%q) = (%d,%d), want (0,0)", raw, r, w)
		}
	}
}
