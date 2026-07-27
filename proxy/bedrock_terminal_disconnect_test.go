package proxy

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

// Round 13 follow-up. An adversarial review of the round-13 client-disconnect fix
// found TWO holes it left open. Both are about the same thing: the fix only
// inspected the return value of Write on the CONTENT chunks, and that is not the
// only place a departed client is observable.
//
// HOLE 1 — the terminal chunk was never checked.
// bedrockOpenAIStreamConv.finish (bedrock_openai.go) writes the terminal
// finish_reason+usage chunk and the [DONE] sentinel and DISCARDED both results:
//
//	fmt.Fprintf(w, "data: %s\n\n", data)
//	fmt.Fprintf(w, "data: [DONE]\n\n")
//
// so a client that hangs up after the last content chunk — or a stream carrying
// only usage/control events, where finish performs the FIRST client write — still
// reached recordBedrockSuccess. That books a success, meters tokens against the
// customer key, and calls pool.RecordSuccess, for a response the client never
// received. This is the mirror image of the defect round 13 set out to fix:
// round 13 stopped over-penalising the account, and left a path that
// over-CREDITS it.
//
// HOLE 2 — a flush failure was invisible.
// net/http's ResponseWriter.Write succeeds into the connection's bufio.Writer,
// and the socket error can surface only on flush. net/http's own implementation
// throws that error away:
//
//	func (w *response) Flush() { w.FlushError() }
//
// (verified in the local go1.25.6 source). So a disconnect that happens between
// chunks left every Write returning nil, and the stream was booked as a clean,
// billed completion. http.NewResponseController surfaces it.
//
// These tests use writers that accept content writes and fail only at the
// terminal write / at flush, which is exactly what the round-13 tests could not
// see: their writer failed on the very first Write.

// terminalFailWriter accepts the first n body writes and fails every write after
// that, modelling a client that hangs up once the answer is nearly delivered.
type terminalFailWriter struct {
	header    http.Header
	accept    int
	writes    int
	failed    int
	flushed   int
	flushFail bool
}

func (t *terminalFailWriter) Header() http.Header {
	if t.header == nil {
		t.header = make(http.Header)
	}
	return t.header
}

func (t *terminalFailWriter) Write(b []byte) (int, error) {
	t.writes++
	if t.writes > t.accept {
		t.failed++
		return 0, errors.New("write: broken pipe")
	}
	return len(b), nil
}

func (t *terminalFailWriter) WriteHeader(int) {}

// FlushError is what http.NewResponseController prefers, so this models the real
// net/http behaviour where the socket error surfaces at flush time.
func (t *terminalFailWriter) FlushError() error {
	t.flushed++
	if t.flushFail {
		return errors.New("flush: connection reset by peer")
	}
	return nil
}

func (t *terminalFailWriter) Flush() { _ = t.FlushError() }

// HOLE 1, native OpenAI surface: the client accepts the content chunks and then
// goes away, so only finish()'s writes fail. Before the fix this was recorded as
// a success and billed.
func TestBedrockOpenAITerminalDisconnectIsNotBilledAsSuccess(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), true)
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	// The fixture stream yields one text delta -> exactly one content chunk is
	// written. Accept it, then fail: finish()'s two writes are the ones that die.
	w := &terminalFailWriter{accept: 1}
	if err := h.invokeBedrockOpenAIStream(w, nil, p); err != nil {
		t.Fatalf("a client disconnect must not be returned as an error: %v", err)
	}

	if w.failed == 0 {
		t.Fatal("no write failed: the fixture never reached the terminal write, so " +
			"this test proves nothing")
	}
	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess {
		t.Errorf("successRequests moved %d -> %d: the terminal chunk and [DONE] "+
			"never reached the client, yet the request was booked as a success "+
			"(pool.RecordSuccess also clears the account's error state)",
			beforeSuccess, got)
	}
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens {
		t.Errorf("totalTokens moved %d -> %d: the customer key was metered for a "+
			"response the client never received", beforeTokens, got)
	}
}

// HOLE 1, Converse->OpenAI surface: same defect, second call site
// (bedrock_converse.go). Round 13 fixed four write sites but both finish() call
// sites shared the same unchecked helper, so both surfaces need pinning.
func TestBedrockConverseOpenAITerminalDisconnectIsNotBilledAsSuccess(t *testing.T) {
	h, p, srv := newConversePartialStreamFixture(t, converseStreamComplete())
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	w := &terminalFailWriter{accept: 1}
	if err := h.invokeBedrockConverseOpenAIStream(w, nil, p); err != nil {
		t.Fatalf("a client disconnect must not be returned as an error: %v", err)
	}

	if w.failed == 0 {
		t.Fatal("no write failed: fixture never reached the terminal write")
	}
	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess {
		t.Errorf("successRequests moved %d -> %d: Converse->OpenAI terminal "+
			"disconnect booked as a success", beforeSuccess, got)
	}
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens {
		t.Errorf("totalTokens moved %d -> %d: metered a response the client never "+
			"received", beforeTokens, got)
	}
}

// HOLE 2: every Write succeeds (into the buffer) and the disconnect surfaces only
// at flush. Checking Write alone cannot see this, so the stream was booked as a
// clean billed completion.
func TestBedrockFlushFailureIsTreatedAsClientGone(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), false)
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)
	beforeErrors := bedrockAccountErrorCount(h, p.account.ID)

	// accept every write; fail at flush.
	w := &terminalFailWriter{accept: 1 << 30, flushFail: true}
	if err := h.invokeBedrockStream(w, nil, p); err != nil {
		t.Fatalf("a flush-time disconnect must not be returned as an error: %v", err)
	}

	if w.flushed == 0 {
		t.Fatal("flush was never attempted: the handler does not flush this " +
			"writer, so this test proves nothing about flush errors")
	}
	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess {
		t.Errorf("successRequests moved %d -> %d: the client vanished and the "+
			"error surfaced only at flush (net/http's Flush() discards it), so a "+
			"stream nobody received was billed as a clean completion",
			beforeSuccess, got)
	}
	// It is also not the account's fault, so it must not be penalised either.
	if got := bedrockAccountErrorCount(h, p.account.ID); got != beforeErrors {
		t.Errorf("account error count moved %d -> %d: a client-side flush failure "+
			"was charged to the account", beforeErrors, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed {
		t.Errorf("failedRequests moved %d -> %d: a client disconnect logged as an "+
			"upstream failure", beforeFailed, got)
	}
}

// Control: a writer that accepts everything and flushes cleanly must still be
// billed as a success. Without this, "treat every flush as a disconnect" would
// satisfy the tests above while destroying all Bedrock billing.
func TestBedrockCleanFlushStillRecordsSuccess(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), false)
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	w := &terminalFailWriter{accept: 1 << 30, flushFail: false}
	if err := h.invokeBedrockStream(w, nil, p); err != nil {
		t.Fatalf("clean stream returned an error: %v", err)
	}

	if w.flushed == 0 {
		t.Fatal("flush never attempted; control does not exercise the flush path")
	}
	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess+1 {
		t.Errorf("successRequests = %d, want %d: a fully delivered stream must "+
			"still be recorded as a success", got, beforeSuccess+1)
	}
	// fixture reports 7 in / 4 out.
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens+11 {
		t.Errorf("totalTokens = %d, want %d: a delivered stream must still meter "+
			"its tokens", got, beforeTokens+11)
	}
}

// Control: an OpenAI stream fully delivered to a live client must still be
// billed, including finish()'s terminal chunk. Pins that making finish() report
// errors did not turn every clean stream into a swallowed disconnect.
func TestBedrockOpenAIFullyDeliveredStreamStillRecordsSuccess(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), true)
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	w := &terminalFailWriter{accept: 1 << 30}
	if err := h.invokeBedrockOpenAIStream(w, nil, p); err != nil {
		t.Fatalf("clean OpenAI stream returned an error: %v", err)
	}

	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess+1 {
		t.Errorf("successRequests = %d, want %d: a fully delivered OpenAI stream "+
			"must still be recorded as a success", got, beforeSuccess+1)
	}
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens+11 {
		t.Errorf("totalTokens = %d, want %d: a delivered OpenAI stream must still "+
			"meter its tokens", got, beforeTokens+11)
	}
}
