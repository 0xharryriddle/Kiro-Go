package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Responses API is the THIRD streaming route (alongside handleClaudeStream
// and handleOpenAIStream). Both of those terminated their streams correctly only
// after being fixed; this route was never examined.
//
// It does emit `response.failed` on a mid-stream failure, which is better than
// the other two were, but it never writes the `[DONE]` sentinel on ANY failure
// path (the success path does, at responses_handler.go:612). An SSE client that
// keys completion off `[DONE]` — which is the documented terminator for this
// wire format, and what the success path teaches it to expect — is left waiting
// on a stream that will never produce one.
func TestResponsesStreamSendsDoneOnMidStreamFailure(t *testing.T) {
	h := setupMidStreamFailureHandler(t, midStreamLongText)

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, midStreamPayload(), "claude-opus-4.6", false, 5, "",
		"resp_test", &ResponsesRequest{Model: "claude-opus-4.6"}, json.RawMessage(`"hi"`), false,
		nil, false)

	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Skipf("upstream failed before the stream started; nothing to terminate\nbody:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("responses stream reported failure but never sent [DONE]\nbody:\n%s", body)
	}
}

// A mid-stream failure must still be attributed to the account that caused it.
// The other two routes call handleAccountFailure unconditionally; this route
// calls it only inside the !responseStarted branch, so a failure that happens
// AFTER bytes reached the client never counts against the account. A degraded
// account therefore keeps getting selected, because the pool never learns it is
// failing mid-stream.
func TestResponsesStreamRecordsAccountFailureMidStream(t *testing.T) {
	h := setupMidStreamFailureHandler(t, midStreamLongText)

	before := h.pool.Diagnostics()
	var beforeErrs int
	for _, d := range before {
		if d.ID == "only" {
			beforeErrs = d.ErrorCount
		}
	}

	rec := httptest.NewRecorder()
	h.handleResponsesStream(rec, midStreamPayload(), "claude-opus-4.6", false, 5, "",
		"resp_test", &ResponsesRequest{Model: "claude-opus-4.6"}, json.RawMessage(`"hi"`), false,
		nil, false)

	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Skipf("upstream failed before the stream started\nbody:\n%s", body)
	}

	after := h.pool.Diagnostics()
	var afterErrs int
	for _, d := range after {
		if d.ID == "only" {
			afterErrs = d.ErrorCount
		}
	}
	if afterErrs <= beforeErrs {
		t.Fatalf("mid-stream failure was not attributed to the account: errorCount %d -> %d", beforeErrs, afterErrs)
	}
}
