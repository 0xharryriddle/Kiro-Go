package proxy

import (
	"bytes"
	"strings"
	"testing"
)

// The Kiro stream reader must recognise the SAME set of failure frames as the
// sibling Bedrock reader (bedrock_eventstream.go). It originally matched only
// `:message-type: exception`, so a `:message-type: error` frame — which the
// Bedrock path does treat as a failure — was silently discarded on the Kiro
// path. The same upstream condition was observable on one surface and invisible
// on the other, which is exactly the kind of divergence that makes a bug
// reproduce "only sometimes".
func TestFailureFrameDetectionMatchesBedrockReader(t *testing.T) {
	cases := []struct {
		name      string
		build     func(*bytes.Buffer)
		eventType string
		want      bool
	}{
		{
			name: "message-type exception",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":message-type", "exception")
			},
			want: true,
		},
		{
			name: "message-type error",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":message-type", "error")
			},
			want: true,
		},
		{
			name: "exception-type present alone",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":exception-type", "ThrottlingException")
			},
			want: true,
		},
		{
			name: "event-type exception",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":event-type", "exception")
			},
			eventType: "exception",
			want:      true,
		},
		{
			name: "event-type error",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":event-type", "error")
			},
			eventType: "error",
			want:      true,
		},
		// The negative cases matter more than the positives: a false positive
		// here silently drops a customer's output.
		{
			name: "normal event frame",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":message-type", "event")
				writeStringHeader(h, ":event-type", "assistantResponseEvent")
			},
			eventType: "assistantResponseEvent",
			want:      false,
		},
		{
			name: "no message-type at all",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":event-type", "assistantResponseEvent")
			},
			eventType: "assistantResponseEvent",
			want:      false,
		},
		{
			name: "empty exception-type is not a failure",
			build: func(h *bytes.Buffer) {
				writeStringHeader(h, ":message-type", "event")
				writeStringHeader(h, ":exception-type", "")
			},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h bytes.Buffer
			c.build(&h)
			if got := isUpstreamFailureFrame(h.Bytes(), c.eventType); got != c.want {
				t.Fatalf("isUpstreamFailureFrame = %v, want %v", got, c.want)
			}
		})
	}
}

// A failure frame can still carry a usage block, and the upstream charges for
// those tokens whether or not the frame reported an error. The first version of
// the detection `continue`d before updateTokensFromEvent ran, so billing
// silently stopped counting them — a revenue-losing accounting hole introduced
// by an observability change.
func TestFailureFrameStillCountsUsage(t *testing.T) {
	var h bytes.Buffer
	writeStringHeader(&h, ":message-type", "exception")
	writeStringHeader(&h, ":exception-type", "ThrottlingException")

	var stream bytes.Buffer
	writeFrameWithHeaders(&stream, h.Bytes(),
		`{"message":"rate exceeded","usage":{"inputTokens":123,"outputTokens":45}}`)

	gotIn, gotOut := -1, -1
	cb := KiroStreamCallback{
		OnComplete: func(in, out int) { gotIn, gotOut = in, out },
	}
	if err := parseEventStream(&stream, &cb); err != nil {
		t.Fatalf("parseEventStream: %v", err)
	}
	if gotIn != 123 || gotOut != 45 {
		t.Fatalf("usage on a failure frame was not counted: in=%d out=%d, want 123/45", gotIn, gotOut)
	}
}

// The failure label is an upstream-controlled event-stream string header, which
// the 16-bit length field lets reach ~64 KiB and which may contain newlines.
// Logging it verbatim let a hostile or malfunctioning upstream write tens of
// kilobytes per failed frame into the operator's log, and forge extra log lines
// with embedded newlines. The bound applies to the LOG; the observer still
// receives the full value so a caller can classify on it.
func TestLoggedFailureLabelIsBounded(t *testing.T) {
	hostile := strings.Repeat("x", 60000)
	if got := truncateForLog(hostile, maxLoggedExceptionType); len(got) > maxLoggedExceptionType+len("...(truncated)") {
		t.Fatalf("truncateForLog did not bound the label: len=%d", len(got))
	}
	if !strings.HasSuffix(truncateForLog(hostile, maxLoggedExceptionType), "...(truncated)") {
		t.Fatal("a shortened label must be marked, or it reads as the whole value")
	}
	// Short labels must pass through untouched — the common case is a real
	// exception name like "ThrottlingException".
	if got := truncateForLog("ThrottlingException", maxLoggedExceptionType); got != "ThrottlingException" {
		t.Fatalf("short label was altered: %q", got)
	}
}

// writeFrameWithHeaders frames a raw header block plus payload.
func writeFrameWithHeaders(buf *bytes.Buffer, headerBytes []byte, payload string) {
	body := []byte(payload)
	total := 12 + len(headerBytes) + len(body) + 4
	prelude := make([]byte, 12)
	prelude[0] = byte(total >> 24)
	prelude[1] = byte(total >> 16)
	prelude[2] = byte(total >> 8)
	prelude[3] = byte(total)
	hl := len(headerBytes)
	prelude[4] = byte(hl >> 24)
	prelude[5] = byte(hl >> 16)
	prelude[6] = byte(hl >> 8)
	prelude[7] = byte(hl)
	buf.Write(prelude)
	buf.Write(headerBytes)
	buf.Write(body)
	buf.Write([]byte{0, 0, 0, 0})
}
