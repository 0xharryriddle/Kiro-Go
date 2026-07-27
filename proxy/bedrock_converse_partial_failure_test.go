package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// c65c161 fixed the mid-stream accounting contract on THREE Bedrock streaming
// paths: invokeBedrockStream (bedrock.go:358), invokeBedrockOpenAIStream
// (bedrock_openai.go:744), and invokeBedrockConverseAnthropicStream
// (bedrock_converse.go:788). Each now calls recordBedrockPartialFailure when a
// stream breaks after client bytes.
//
// The FOURTH path was missed. invokeBedrockConverseOpenAIStream
// (bedrock_converse.go:882-891) still does:
//
//	if streamErr != nil && !oconv.started {
//	    return streamErr                        // pre-byte: failover, correct
//	}
//	if streamErr != nil {
//	    logger.Warnf(...)                       // partial: log only
//	}                                           // <- falls through
//	oconv.finish(w, flusher)
//	h.recordBedrockSuccess(p, ...)              // <- records a SUCCESS
//
// so a Converse stream serving an OpenAI customer that dies mid-flight is booked
// as a success. The consequence is the one c65c161 documented: pool.RecordSuccess
// CLEARS the account's error count and cooldown (pool/account.go:818), so an
// account throwing repeated mid-stream Converse exceptions resets its own health
// on every failure, keeps looking healthy, and keeps being selected. It also
// meters tokens from a truncated stream and writes a success request-log entry
// for a request the client saw fail.
//
// This drives the REAL entrypoint (not the helper in isolation, which is what let
// the missed call site hide from TestBedrockPartialStreamFailureIsNotRecordedAsSuccess):
// a hermetic upstream serves two good Converse frames and then an exception
// frame, so oconv.started is true and streamErr is non-nil — exactly the partial
// case.
func TestBedrockConverseOpenAIPartialStreamIsNotRecordedAsSuccess(t *testing.T) {
	h, p, srv := newConversePartialStreamFixture(t, converseStreamPartialThenException())
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	rec := httptest.NewRecorder()
	if err := h.invokeBedrockConverseOpenAIStream(rec, rec, p); err != nil {
		t.Fatalf("partial stream must not return an error (client already has bytes, "+
			"failover is impossible): %v", err)
	}

	// Sanity: the client really did receive bytes, so this IS the partial case and
	// not an accidental pre-byte failure that would pass for the wrong reason.
	if rec.Body.Len() == 0 {
		t.Fatal("no client bytes were written: the fixture did not exercise the " +
			"partial-stream path, so this test proves nothing")
	}

	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess {
		t.Errorf("successRequests moved %d -> %d: a Converse stream that broke "+
			"mid-flight was counted as a SUCCESS. pool.RecordSuccess resets the "+
			"account's error count and cooldown, so an account throwing repeated "+
			"mid-stream exceptions clears its own health and stays selectable",
			beforeSuccess, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed+1 {
		t.Errorf("failedRequests = %d, want %d: a mid-stream failure must be "+
			"recorded as a failure so pool health reflects reality",
			got, beforeFailed+1)
	}
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens {
		t.Errorf("totalTokens moved %d -> %d: usage was metered from a truncated "+
			"stream whose terminal usage event never arrived", beforeTokens, got)
	}
}

// Positive control: a Converse stream that completes normally must still record a
// success and still meter its tokens. Without this, "always record a failure"
// would satisfy the test above while breaking all Converse OpenAI accounting.
func TestBedrockConverseOpenAICompleteStreamStillRecordsSuccess(t *testing.T) {
	h, p, srv := newConversePartialStreamFixture(t, converseStreamComplete())
	defer srv.Close()

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	rec := httptest.NewRecorder()
	if err := h.invokeBedrockConverseOpenAIStream(rec, rec, p); err != nil {
		t.Fatalf("clean stream returned an error: %v", err)
	}

	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess+1 {
		t.Errorf("successRequests = %d, want %d: a completed stream must still "+
			"count as a success", got, beforeSuccess+1)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed {
		t.Errorf("failedRequests moved %d -> %d: a clean stream must not be "+
			"recorded as a failure", beforeFailed, got)
	}
	// metadata reports 11 in / 5 out.
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens+16 {
		t.Errorf("totalTokens = %d, want %d: a completed stream must still meter "+
			"its tokens", got, beforeTokens+16)
	}
}

// Control: a stream that fails BEFORE any client byte must still return an error
// so the dispatch loop can fail over to another account. This pins that the fix
// does not convert a failover-able error into a swallowed partial.
func TestBedrockConverseOpenAIPreByteFailureStillFailsOver(t *testing.T) {
	h, p, srv := newConversePartialStreamFixture(t, converseStreamImmediateException())
	defer srv.Close()

	rec := httptest.NewRecorder()
	err := h.invokeBedrockConverseOpenAIStream(rec, rec, p)
	if err == nil {
		t.Fatal("a stream that failed before any client byte must return an error " +
			"so the dispatch loop fails over to a healthy account")
	}
}

// newConversePartialStreamFixture wires a hermetic Bedrock upstream that serves
// the given event-stream bytes, and returns a Handler plus forwardParams aimed at
// it. Mirrors bedrock_region_test.go's client injection (explicit no-proxy
// transport so http.ProxyFromEnvironment's process-wide sync.Once is untouched).
func newConversePartialStreamFixture(t *testing.T, stream []byte) (*Handler, forwardParams, *httptest.Server) {
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

	acctID := "acct-converse-partial-" + t.Name()
	h := &Handler{pool: accountpool.GetPool()}
	p := forwardParams{
		account: &config.Account{
			ID:                 acctID,
			AuthMethod:         "bedrock",
			BedrockAPIKey:      "bedrock-test-key",
			Region:             "us-east-1",
			BedrockUseConverse: true,
			BedrockModelMap:    map[string]string{"nova-pro": "amazon.nova-pro-v1:0"},
		},
		model:    "nova-pro",
		endpoint: "openai",
		body:     []byte(`{"model":"nova-pro","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	t.Cleanup(func() { clearBedrockRegionRoutes(acctID) })
	return h, p, srv
}

// roundTripToTestServer redirects any Bedrock URL to the local test server while
// leaving the signed request otherwise intact. The real endpoint host is what
// newBedrockRequestForURL validates, so the request must be BUILT against the AWS
// host and only then redirected at transport level.
type roundTripToTestServer struct {
	target string
	inner  http.Transport
}

func (rt *roundTripToTestServer) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := http.NewRequest(req.Method, rt.target, req.Body)
	if err != nil {
		return nil, err
	}
	target.Header = req.Header
	rt.inner.Proxy = nil
	return rt.inner.RoundTrip(target)
}

// converseStreamPartialThenException: two good frames (so client bytes flow),
// then an exception frame — the partial-failure case.
func converseStreamPartialThenException() []byte {
	var b bytes.Buffer
	b.Write(converseFrame("messageStart", `{"role":"assistant"}`))
	b.Write(converseFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"partial"}}`))
	b.Write(buildFrame(map[string]string{
		":message-type":   "exception",
		":exception-type": "ModelStreamErrorException",
		":content-type":   "application/json",
	}, []byte(`{"message":"stream broke mid-flight"}`)))
	return b.Bytes()
}

// converseStreamComplete: a well-formed stream through messageStop + metadata.
func converseStreamComplete() []byte {
	var b bytes.Buffer
	b.Write(converseFrame("messageStart", `{"role":"assistant"}`))
	b.Write(converseFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"all good"}}`))
	b.Write(converseFrame("contentBlockStop", `{"contentBlockIndex":0}`))
	b.Write(converseFrame("messageStop", `{"stopReason":"end_turn"}`))
	b.Write(converseFrame("metadata", `{"usage":{"inputTokens":11,"outputTokens":5}}`))
	return b.Bytes()
}

// converseStreamImmediateException: an exception on the FIRST frame, so no client
// byte is ever written and failover must remain possible.
func converseStreamImmediateException() []byte {
	return buildFrame(map[string]string{
		":message-type":   "exception",
		":exception-type": "ThrottlingException",
		":content-type":   "application/json",
	}, []byte(`{"message":"slow down"}`))
}
