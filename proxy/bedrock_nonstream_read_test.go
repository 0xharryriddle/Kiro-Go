package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Every non-streaming Bedrock call site read its response body as:
//
//	respBody, _ := io.ReadAll(resp.Body)
//
// Two defects in one line.
//
// 1. The DISCARDED error. When Bedrock answers 200 and the body then fails
//    partway through, io.ReadAll returns the partial bytes AND an error. With the
//    error dropped, those partial bytes were written to the client with HTTP 200
//    and recordBedrockSuccess billed the request. Nothing had reached the client
//    when the failure happened, so per CLAUDE.md this must return an error and
//    let the dispatch loop fail over to a healthy account. Instead the customer
//    got malformed JSON reported as success, and no other account was tried.
//
// 2. The UNBOUNDED read. Allocation was proportional to whatever the upstream
//    sent, with no ceiling — while the sibling custom_api forwarder bounds the
//    same kind of read at 32 MiB and checks its error
//    (custom_api_forward.go:413).
//
// These tests pin the read helper that all four non-stream sites now share
// (native Anthropic, native OpenAI, Converse→Anthropic, Converse→OpenAI).

// failingBody yields prefix, then a sentinel error — the shape of a connection
// that dies mid-body after a 200 header.
type failingBody struct {
	prefix []byte
	off    int
	err    error
}

func (f *failingBody) Read(p []byte) (int, error) {
	if f.off < len(f.prefix) {
		n := copy(p, f.prefix[f.off:])
		f.off += n
		return n, nil
	}
	return 0, f.err
}

func (f *failingBody) Close() error { return nil }

func TestBedrockNonStreamReadErrorIsNotSilentlyServed(t *testing.T) {
	sentinel := errors.New("connection reset mid-body")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &failingBody{prefix: []byte(`{"content":`), err: sentinel},
	}

	body, err := readBedrockResponseBody(resp)
	if err == nil {
		t.Fatalf("read error was swallowed: got body %q with nil error. The caller "+
			"then writes those partial bytes to the client as HTTP 200 and bills a "+
			"success, and no healthy account is tried", body)
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("error did not preserve the underlying cause: %v", err)
	}
}

// The read must be bounded, so a hostile or malfunctioning upstream cannot force
// unbounded allocation.
func TestBedrockNonStreamReadIsBounded(t *testing.T) {
	// A reader that would happily produce far more than the ceiling.
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(&infiniteReader{b: 'x'}),
	}

	if _, err := readBedrockResponseBody(resp); err == nil {
		t.Fatalf("an unbounded upstream body was accepted; the read must be capped "+
			"at %d bytes like the sibling custom_api forwarder", bedrockMaxNonStreamResponseBytes)
	}
}

// Positive control: a normal body must still be returned intact. Without this,
// "always fail" would look like a valid fix while breaking every Bedrock call.
func TestBedrockNonStreamNormalBodyStillReadsFully(t *testing.T) {
	want := `{"usage":{"input_tokens":11,"output_tokens":3},"content":[]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(want)),
	}

	got, err := readBedrockResponseBody(resp)
	if err != nil {
		t.Fatalf("a normal response body must read cleanly, got %v", err)
	}
	if string(got) != want {
		t.Fatalf("body mismatch:\n got %s\nwant %s", got, want)
	}
}

// Positive control: a body exactly at the ceiling is still accepted, so the
// bound is off-by-one correct rather than rejecting legitimate large responses.
func TestBedrockNonStreamBodyExactlyAtLimitIsAccepted(t *testing.T) {
	exact := strings.Repeat("y", bedrockMaxNonStreamResponseBytes)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(exact)),
	}

	got, err := readBedrockResponseBody(resp)
	if err != nil {
		t.Fatalf("a body exactly at the %d-byte ceiling must be accepted, got %v",
			bedrockMaxNonStreamResponseBytes, err)
	}
	if len(got) != bedrockMaxNonStreamResponseBytes {
		t.Fatalf("truncated an at-limit body: got %d bytes, want %d",
			len(got), bedrockMaxNonStreamResponseBytes)
	}
}

// infiniteReader never ends, so only an explicit bound can stop the read.
type infiniteReader struct{ b byte }

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}
