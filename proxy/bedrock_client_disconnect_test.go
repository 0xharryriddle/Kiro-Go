package proxy

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// A client that hangs up mid-answer is not an account fault, and the sibling
// custom_api forwarder already encodes exactly that for the identical situation:
//
//	if _, werr := w.Write(chunk); werr != nil {
//	    return nil // client gone; nothing to fail over to
//	}
//
// (custom_api_forward.go:445-447) — no account penalty, no failover, no billing.
//
// The Bedrock streaming paths did the opposite. They write to the client from
// inside the event-stream reader's callback, and the reader propagates a callback
// error verbatim (bedrock_eventstream.go:53-57), so a failed CLIENT write reached
// the caller as the same opaque `streamErr` as an upstream Bedrock fault and was
// then charged to the ACCOUNT:
//
//   - bedrock.go:338-342 sets streamedAny=true BEFORE the write, so a disconnect
//     lands in the partial branch -> recordBedrockPartialFailure -> pool.RecordError.
//     Three departing customers cool the account for a minute
//     (pool/account.go:878) and five open its circuit breaker for 30s (:149).
//   - bedrock_openai.go:586-596 sets started=true only AFTER a successful write, so
//     a disconnect on the FIRST chunk leaves started=false and the error is
//     returned to the dispatch loop, which marks the account excluded and calls
//     handleAccountFailure, then retries the next account — whose write to the same
//     dead client fails identically. One departed client walks the pool and
//     penalises every account it touches.
//
// These tests drive the REAL entrypoints with a ResponseWriter whose Write always
// fails, which is what a disconnected client looks like from inside the handler.
func TestBedrockClientDisconnectDoesNotPenaliseAccount(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), false)
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)
	beforeErrors := bedrockAccountErrorCount(h, p.account.ID)

	w := &failingResponseWriter{}
	if err := h.invokeBedrockStream(w, nil, p); err != nil {
		t.Fatalf("a client disconnect must not be returned as an error (there is "+
			"nobody left to fail over for, and returning it makes the dispatch loop "+
			"penalise the next account too): %v", err)
	}

	// Sanity: the fixture really did attempt a client write, so this exercises the
	// disconnect path rather than passing for some unrelated reason.
	if w.attempts == 0 {
		t.Fatal("no client write was attempted: the fixture did not exercise the " +
			"disconnect path, so this test proves nothing")
	}

	if got := bedrockAccountErrorCount(h, p.account.ID); got != beforeErrors {
		t.Errorf("account error count moved %d -> %d: a customer hanging up was "+
			"charged to the account. At 3 errors pool.RecordError cools it for a "+
			"minute and at 5 the circuit breaker opens for 30s, so a few departing "+
			"clients park a perfectly healthy account", beforeErrors, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed {
		t.Errorf("failedRequests moved %d -> %d: a client disconnect was logged as "+
			"an upstream failure, so operator error dashboards blame Bedrock for "+
			"customers closing their connection", beforeFailed, got)
	}
	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess {
		t.Errorf("successRequests moved %d -> %d: a stream the client never received "+
			"must not be booked as a success either", beforeSuccess, got)
	}
}

// The OpenAI surface is the more damaging half: started is set only after a
// successful write, so a disconnect on the first chunk is indistinguishable from a
// pre-stream upstream fault and is returned to the dispatch loop as a failover
// signal — walking the pool and penalising each account against the same dead
// client.
func TestBedrockOpenAIClientDisconnectDoesNotTriggerFailover(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), true)
	defer srv.Close()

	beforeFailed := atomic.LoadInt64(&h.failedRequests)
	beforeErrors := bedrockAccountErrorCount(h, p.account.ID)

	w := &failingResponseWriter{}
	err := h.invokeBedrockOpenAIStream(w, nil, p)
	if err != nil {
		t.Fatalf("a client disconnect must not be returned as a failover error: the "+
			"dispatch loop marks the account excluded, calls handleAccountFailure, "+
			"then retries the next account whose write to the same dead client fails "+
			"identically — one departed client penalises the whole pool: %v", err)
	}
	if w.attempts == 0 {
		t.Fatal("no client write was attempted: fixture did not exercise the path")
	}
	if got := bedrockAccountErrorCount(h, p.account.ID); got != beforeErrors {
		t.Errorf("account error count moved %d -> %d for a client disconnect",
			beforeErrors, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed {
		t.Errorf("failedRequests moved %d -> %d for a client disconnect",
			beforeFailed, got)
	}
}

// Control: a genuine UPSTREAM fault after client bytes must STILL be recorded as a
// partial failure. Without this, "never penalise on stream error" would satisfy the
// tests above while undoing c65c161 and letting an account that throws repeated
// mid-stream exceptions clear its own health.
func TestBedrockUpstreamPartialFailureStillPenalisesAccount(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativePartialThenException(t), false)
	defer srv.Close()

	beforeFailed := atomic.LoadInt64(&h.failedRequests)
	beforeErrors := bedrockAccountErrorCount(h, p.account.ID)

	rec := httptest.NewRecorder()
	if err := h.invokeBedrockStream(rec, rec, p); err != nil {
		t.Fatalf("partial upstream failure must not fail over: %v", err)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("no client bytes written: not the partial case")
	}
	if got := bedrockAccountErrorCount(h, p.account.ID); got != beforeErrors+1 {
		t.Errorf("account error count = %d, want %d: a genuine mid-stream Bedrock "+
			"exception must still count against the account", got, beforeErrors+1)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed+1 {
		t.Errorf("failedRequests = %d, want %d: a genuine upstream fault must still "+
			"be recorded as a failure", got, beforeFailed+1)
	}
}

// Control: a genuine upstream fault BEFORE any client byte must still return an
// error so the dispatch loop fails over to a healthy account.
func TestBedrockUpstreamPreByteFailureStillFailsOver(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeImmediateException(), false)
	defer srv.Close()

	rec := httptest.NewRecorder()
	if err := h.invokeBedrockStream(rec, rec, p); err == nil {
		t.Fatal("an upstream fault before any client byte must return an error so " +
			"the dispatch loop fails over to a healthy account")
	}
}

// Control: a clean stream to a healthy client must still be billed as a success.
func TestBedrockCleanStreamToLiveClientStillSucceeds(t *testing.T) {
	h, p, srv := newBedrockDisconnectFixture(t, bedrockNativeStreamComplete(t), false)
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	rec := httptest.NewRecorder()
	if err := h.invokeBedrockStream(rec, rec, p); err != nil {
		t.Fatalf("clean stream returned an error: %v", err)
	}
	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess+1 {
		t.Errorf("successRequests = %d, want %d: a completed stream must still be "+
			"recorded as a success", got, beforeSuccess+1)
	}
	// message_start reports 7 input tokens, message_delta reports 4 output.
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens+11 {
		t.Errorf("totalTokens = %d, want %d: a completed stream must still meter its "+
			"tokens", got, beforeTokens+11)
	}
}

// Unit-level pin on the classifier, so the four-way disposition stays explicit
// even if the call sites are refactored.
func TestClassifyBedrockStreamOutcome(t *testing.T) {
	upstream := errors.New("bedrock stream: ThrottlingException: slow down")
	cases := []struct {
		name    string
		err     error
		started bool
		want    bedrockStreamDisposition
	}{
		{"clean", nil, true, bedrockStreamComplete},
		{"upstream pre-byte", upstream, false, bedrockStreamFailover},
		{"upstream partial", upstream, true, bedrockStreamPartialFailure},
		// started=false must NOT turn a disconnect into a failover signal.
		{"client gone first chunk", clientGone(errors.New("broken pipe")), false, bedrockStreamClientGone},
		{"client gone mid-stream", clientGone(errors.New("broken pipe")), true, bedrockStreamClientGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBedrockStreamOutcome(tc.err, tc.started); got != tc.want {
				t.Errorf("classify(%v, started=%v) = %v, want %v", tc.err, tc.started, got, tc.want)
			}
		})
	}
}

// failingResponseWriter is a ResponseWriter whose body writes always fail, which
// is what a client that has gone away looks like from inside a handler.
type failingResponseWriter struct {
	header   http.Header
	attempts int
}

func (f *failingResponseWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}

func (f *failingResponseWriter) Write(b []byte) (int, error) {
	f.attempts++
	return 0, errors.New("write: broken pipe")
}

func (f *failingResponseWriter) WriteHeader(int) {}

// bedrockAccountErrorCount reads the pool's recorded error count for an account
// through the exported diagnostics view (errorCounts itself is unexported).
func bedrockAccountErrorCount(h *Handler, accountID string) int {
	for _, d := range h.pool.DiagnosticsFor([]config.Account{{ID: accountID}}) {
		if d.ID == accountID {
			return d.ErrorCount
		}
	}
	return -1
}

// newBedrockDisconnectFixture wires a hermetic Bedrock upstream serving the given
// native event-stream bytes, and returns a Handler plus forwardParams aimed at it.
// Mirrors newConversePartialStreamFixture's client injection; openaiSurface picks
// which endpoint label the params carry.
func newBedrockDisconnectFixture(t *testing.T, stream []byte, openaiSurface bool) (*Handler, forwardParams, *httptest.Server) {
	t.Helper()
	// config.Init must precede GetPool(): Reload reads config, and an
	// uninitialized config makes GetEnabledAccounts nil-deref.
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stream)
	}))

	prevClient := bedrockHTTPClientFor
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: &roundTripToTestServer{target: srv.URL}}
	}
	t.Cleanup(func() { bedrockHTTPClientFor = prevClient })

	acctID := "acct-bedrock-disconnect-" + t.Name()
	h := &Handler{pool: accountpool.GetPool()}
	endpoint := "anthropic"
	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`)
	if openaiSurface {
		endpoint = "openai"
	}
	p := forwardParams{
		account: &config.Account{
			ID:              acctID,
			AuthMethod:      "bedrock",
			BedrockAPIKey:   "bedrock-test-key",
			Region:          "us-east-1",
			BedrockModelMap: map[string]string{"claude-test": "anthropic.claude-test-v1:0"},
		},
		model:     "claude-test",
		endpoint:  endpoint,
		streaming: true,
		body:      body,
	}
	t.Cleanup(func() { clearBedrockRegionRoutes(acctID) })
	return h, p, srv
}

// bedrockNativeStreamComplete is a well-formed native Anthropic stream carrying
// usage in message_start (7 in) and message_delta (4 out).
func bedrockNativeStreamComplete(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.Write(bedrockTestFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":7}}}`))
	b.Write(bedrockTestFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.Write(bedrockTestFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`))
	b.Write(bedrockTestFrame(t, `{"type":"content_block_stop","index":0}`))
	b.Write(bedrockTestFrame(t, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`))
	b.Write(bedrockTestFrame(t, `{"type":"message_stop"}`))
	return b.Bytes()
}

// bedrockNativePartialThenException delivers real content and then an AWS
// exception frame: a genuine upstream mid-stream fault.
func bedrockNativePartialThenException(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.Write(bedrockTestFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":7}}}`))
	b.Write(bedrockTestFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`))
	b.Write(buildFrame(map[string]string{
		":message-type":   "exception",
		":exception-type": "ModelStreamErrorException",
		":content-type":   "application/json",
	}, []byte(`{"message":"stream broke mid-flight"}`)))
	return b.Bytes()
}

// bedrockNativeImmediateException faults on the first frame, before any client
// byte, so failover must remain possible.
func bedrockNativeImmediateException() []byte {
	return buildFrame(map[string]string{
		":message-type":   "exception",
		":exception-type": "ThrottlingException",
		":content-type":   "application/json",
	}, []byte(`{"message":"slow down"}`))
}
