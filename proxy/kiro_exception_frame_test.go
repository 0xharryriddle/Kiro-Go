package proxy

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// AWS event-stream signals a failure with header `:message-type: exception`
// (plus `:exception-type` naming it), NOT with a `:event-type`. parseEventStream
// reads only `:event-type` via extractEventType, so an exception frame fell
// through to the JSON decode and was `continue`d — silently discarded.
//
// The consequence is indirect but real: a mid-stream upstream failure never
// surfaced as a classified error, so it could only ever reach the client as a
// transport error (status 500 -> "api_error"). Every richer classification the
// Claude error event can express was unreachable on the streaming path.
//
// This pins OBSERVATION only. Deliberately NOT a behaviour change: which
// :exception-type values are fatal versus benign is unknown for Kiro (no real
// exception frame has ever been captured — the trace facility stores assembled
// text, so frame headers are destroyed before capture). Treating a frame as
// fatal on inference risks killing live streams mid-answer on the hot path.
// Recording the headers is what makes the evidence collectable.
func writeExceptionFrame(buf *bytes.Buffer, exceptionType, payload string) {
	// Two string headers: ":message-type" = "exception", ":exception-type" = X.
	var headers bytes.Buffer
	writeStringHeader(&headers, ":message-type", "exception")
	writeStringHeader(&headers, ":exception-type", exceptionType)

	body := []byte(payload)
	headerBytes := headers.Bytes()
	total := 12 + len(headerBytes) + len(body) + 4

	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headerBytes)))
	// prelude CRC is not validated by the parser; zero is accepted.
	buf.Write(prelude)
	buf.Write(headerBytes)
	buf.Write(body)
	buf.Write([]byte{0, 0, 0, 0}) // message CRC, likewise unvalidated
}

// writeStringHeader encodes one event-stream header with a string (type 7) value.
func writeStringHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteByte(byte(len(name)))
	buf.WriteString(name)
	buf.WriteByte(7) // value type: string
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(value)))
	buf.Write(lenBuf)
	buf.WriteString(value)
}

// The parser must REPORT an exception frame's type through the callback, so an
// operator can discover which :exception-type values Kiro actually emits.
func TestParseEventStreamReportsExceptionFrames(t *testing.T) {
	var stream bytes.Buffer
	writeExceptionFrame(&stream, "ThrottlingException", `{"message":"rate exceeded"}`)

	var seenType, seenMessage string
	cb := KiroStreamCallback{
		OnUpstreamException: func(exceptionType string, payload []byte) {
			seenType = exceptionType
			seenMessage = string(payload)
		},
	}
	if err := parseEventStream(&stream, &cb); err != nil {
		t.Fatalf("parseEventStream returned an error for an observed exception frame: %v", err)
	}

	if seenType != "ThrottlingException" {
		t.Fatalf("exception type not reported: got %q, want %q", seenType, "ThrottlingException")
	}
	if seenMessage == "" {
		t.Fatalf("exception payload not reported; an operator needs the message to classify it")
	}
}

// Observation must not become termination. A stream that continues after an
// exception frame must still deliver the text that follows it, because we do not
// know that every exception is fatal.
func TestExceptionFrameDoesNotAbortTheStream(t *testing.T) {
	var stream bytes.Buffer
	writeExceptionFrame(&stream, "SomeUnknownException", `{"message":"who knows"}`)
	writeTestEventFrame(&stream, "assistantResponseEvent", `{"content":"text after the exception"}`)

	var got string
	cb := KiroStreamCallback{
		OnText: func(text string, isThinking bool) { got += text },
	}
	if err := parseEventStream(&stream, &cb); err != nil {
		t.Fatalf("parseEventStream error: %v", err)
	}
	if got != "text after the exception" {
		t.Fatalf("text following an exception frame was lost: got %q", got)
	}
}

// A nil OnUpstreamException must be safe: every existing caller omits it.
func TestExceptionFrameWithNoObserverIsSafe(t *testing.T) {
	var stream bytes.Buffer
	writeExceptionFrame(&stream, "ThrottlingException", `{"message":"rate exceeded"}`)
	writeTestEventFrame(&stream, "assistantResponseEvent", `{"content":"still here"}`)

	var got string
	cb := KiroStreamCallback{
		OnText: func(text string, isThinking bool) { got += text },
	}
	if err := parseEventStream(&stream, &cb); err != nil {
		t.Fatalf("parseEventStream error with nil observer: %v", err)
	}
	if got != "still here" {
		t.Fatalf("stream broke when no exception observer was set: got %q", got)
	}
}

// writeTestEventFrame encodes a normal :event-type frame.
func writeTestEventFrame(buf *bytes.Buffer, eventType, payload string) {
	var headers bytes.Buffer
	writeStringHeader(&headers, ":event-type", eventType)

	body := []byte(payload)
	headerBytes := headers.Bytes()
	total := 12 + len(headerBytes) + len(body) + 4

	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headerBytes)))
	buf.Write(prelude)
	buf.Write(headerBytes)
	buf.Write(body)
	buf.Write([]byte{0, 0, 0, 0})
}
