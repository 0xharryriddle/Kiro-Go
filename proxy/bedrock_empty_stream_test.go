package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// bedrockTestFrame wraps one inner Anthropic event in the {"bytes": base64(...)}
// envelope and the AWS event-stream framing that readBedrockEventStream expects,
// reusing buildFrame from bedrock_eventstream_test.go.
func bedrockTestFrame(t *testing.T, innerJSON string) []byte {
	t.Helper()
	envelope, err := json.Marshal(map[string]string{
		"bytes": base64.StdEncoding.EncodeToString([]byte(innerJSON)),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return buildFrame(map[string]string{
		":message-type": "event",
		":event-type":   "chunk",
		":content-type": "application/json",
	}, envelope)
}

// CLAUDE.md states the failover contract for this provider explicitly:
//
//	"Bedrock errors that happen *before any client bytes* must return an error so
//	 failover works; errors *after* partial streaming are logged, not failed over."
//
// readBedrockEventStream violates the first half. It returns nil for BOTH a clean
// EOF and a truncated prelude:
//
//	if _, err := io.ReadFull(body, prelude); err != nil {
//	    if err == io.EOF || err == io.ErrUnexpectedEOF { return nil }
//
// io.EOF on the FIRST read means the body was empty — no frame ever arrived. That
// is indistinguishable here from "the stream ended normally after N frames", and
// io.ErrUnexpectedEOF means the prelude itself was cut mid-way, which is a
// corrupt stream rather than a complete one.
//
// The caller only fails over when the error is non-nil:
//
//	if streamErr != nil && !streamedAny { return streamErr }
//	...
//	h.recordBedrockSuccess(p, inputTokens, outputTokens, reqStart)
//
// So an HTTP 200 with an empty or pre-first-frame-truncated body produces: zero
// client bytes written, nil returned, NO failover to a healthy account, and a
// zero-token SUCCESS recorded against the account and the customer key.
//
// This test pins the reader's contract at the point the information still exists
// (whether any frame was seen), which is the only place the two cases can be told
// apart.
func TestBedrockEmptyStreamIsNotSilentSuccess(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"completely empty body", nil},
		{"truncated prelude", []byte{0, 0, 0, 8}}, // 4 of the 12 prelude bytes
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events int
			err := readBedrockEventStream(bytes.NewReader(tc.body), func(string, []byte) error {
				events++
				return nil
			})

			if events != 0 {
				t.Fatalf("precondition: expected no events, got %d", events)
			}
			if err == nil {
				t.Fatalf("reader returned nil for a stream that produced NO events "+
					"(%s). The caller only fails over when the error is non-nil, so "+
					"this is reported to the client as a successful empty response, "+
					"a healthy account is never tried, and a zero-token success is "+
					"billed. CLAUDE.md requires a pre-client-byte failure to return "+
					"an error.", tc.name)
			}
		})
	}
}

// Positive control: a stream that DOES deliver frames and then ends cleanly must
// still return nil. Without this, "always error on EOF" would look like a valid
// fix while breaking every successful Bedrock stream.
func TestBedrockCleanEOFAfterFramesStillSucceeds(t *testing.T) {
	frame := bedrockTestFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":7}}}`)

	var events int
	err := readBedrockEventStream(bytes.NewReader(frame), func(_ string, payload []byte) error {
		events++
		if !strings.Contains(string(payload), "message_start") {
			t.Fatalf("unexpected payload: %s", payload)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("clean EOF after %d frame(s) must succeed, got %v", events, err)
	}
	if events != 1 {
		t.Fatalf("expected exactly 1 event, got %d", events)
	}
}

// Positive control: a real mid-stream read failure must still propagate, and must
// not be masked by the new empty-stream error.
func TestBedrockMidStreamReadErrorStillPropagates(t *testing.T) {
	frame := bedrockTestFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":7}}}`)
	sentinel := errors.New("sentinel transport failure")

	r := io.MultiReader(bytes.NewReader(frame), &erroringReader{err: sentinel})

	var events int
	err := readBedrockEventStream(r, func(string, []byte) error {
		events++
		return nil
	})
	if events != 1 {
		t.Fatalf("expected the first frame to be delivered, got %d events", events)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("mid-stream read error was not propagated: got %v", err)
	}
}

type erroringReader struct{ err error }

func (e *erroringReader) Read([]byte) (int, error) { return 0, e.err }

// The Converse reader is a second copy of the same framing loop and carried the
// identical defect, so it is pinned identically. Fixing only the native reader
// would leave every BedrockUseConverse account exposed to the same silent
// empty-success.
func TestBedrockConverseEmptyStreamIsNotSilentSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"completely empty body", nil},
		{"truncated prelude", []byte{0, 0, 0, 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events int
			err := readBedrockConverseEventStream(bytes.NewReader(tc.body), func(string, []byte) error {
				events++
				return nil
			})
			if events != 0 {
				t.Fatalf("precondition: expected no events, got %d", events)
			}
			if err == nil {
				t.Fatalf("converse reader returned nil for a stream with NO events (%s); "+
					"the caller treats that as success, so no failover happens and a "+
					"zero-token success is billed", tc.name)
			}
		})
	}
}

// Positive control for the Converse reader: real frames followed by a clean EOF
// must still succeed.
func TestBedrockConverseCleanEOFAfterFramesStillSucceeds(t *testing.T) {
	frame := buildFrame(map[string]string{
		":message-type": "event",
		":event-type":   "messageStart",
		":content-type": "application/json",
	}, []byte(`{"role":"assistant"}`))

	var events int
	err := readBedrockConverseEventStream(bytes.NewReader(frame), func(evt string, payload []byte) error {
		events++
		if evt != "messageStart" {
			t.Fatalf("unexpected event type %q", evt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("clean EOF after a real frame must succeed, got %v", err)
	}
	if events != 1 {
		t.Fatalf("expected 1 event, got %d", events)
	}
}
