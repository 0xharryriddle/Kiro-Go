package proxy

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// 8f49a4b closed this exact defect for BOTH Bedrock event-stream readers
// (readBedrockEventStream and readBedrockConverseEventStream) by counting frames
// and returning errBedrockEmptyStream when an HTTP 200 carried none. It left the
// THIRD reader — parseEventStream in kiro.go, which serves the main Kiro path and
// therefore essentially all production traffic — with the original behaviour:
//
//	prelude := make([]byte, 12)
//	_, err := io.ReadFull(body, prelude)
//	if err == io.EOF {
//	    break          // kiro.go:745-747 — zero frames is indistinguishable
//	}                  // from a stream that completed normally
//	...
//	return nil
//
// The consequence chain on the Claude streaming path (handler.go:2085 onward),
// where the returned error is the ONLY failover signal:
//
//   - err == nil, so the `if err != nil { excluded[...]; handleAccountFailure }`
//     branch is skipped entirely — no failover to a healthy account;
//   - execution falls through to handler.go:2163-2167, which calls
//     recordSuccessForApiKey (billing the customer), pool.RecordSuccess (which
//     CLEARS the account's error count and cooldown, pool/account.go:818) and
//     UpdateStats;
//   - inputTokens falls back to estimatedInputTokens (handler.go:2144-2146), so
//     the customer is billed a full input-token estimate for a response that
//     produced nothing;
//   - the client receives message_delta with stop_reason "end_turn" — a normal,
//     successful, empty answer.
//
// So an upstream that answers 200 with no events is served to the customer as a
// billed success, and the account that did it has its health reset rather than
// being failed away from.
//
// This test pins the reader's contract at the only point where the information
// still exists: whether any frame was seen at all.
func TestKiroEmptyStreamIsNotSilentSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"completely empty 200 body", []byte{}},
		{"truncated before the first prelude", []byte{0, 0, 0, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawText bool
			err := parseEventStream(bytes.NewReader(tc.body), &KiroStreamCallback{
				OnText: func(string, bool) { sawText = true },
			})
			if sawText {
				t.Fatal("no frame was supplied, so no text can have been delivered")
			}
			if err == nil {
				t.Fatalf("parseEventStream returned nil for a 200 that carried no events. "+
					"On the Claude stream path the nil is the only failover signal, so the "+
					"request is recorded as an account SUCCESS (resetting the account's "+
					"error count via pool.RecordSuccess), the customer key is billed an "+
					"estimated input-token total for zero output, and the client is sent "+
					"stop_reason end_turn as though the empty answer were real (body=%d bytes)",
					len(tc.body))
			}
		})
	}
}

// Positive control 1: a real frame followed by a clean EOF must still succeed and
// still deliver its text. Without this, "always return an error" would satisfy the
// test above while breaking every working stream in production.
func TestKiroStreamWithFramesStillSucceeds(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	})

	var got string
	err := parseEventStream(bytes.NewReader(frame), &KiroStreamCallback{
		OnText: func(text string, isThinking bool) { got += text },
	})
	if err != nil {
		t.Fatalf("a well-formed frame followed by EOF must succeed, got %v", err)
	}
	if got != "hello" {
		t.Fatalf("frame payload not delivered: got %q, want %q", got, "hello")
	}
}

// Positive control 2: the empty-stream error must not mask a genuine mid-stream
// read failure. A frame that announces more bytes than the body carries has to
// surface as a real read error, not as the empty-stream sentinel — otherwise the
// new guard would swallow transport truncation.
func TestKiroTruncatedFrameStillReportsReadError(t *testing.T) {
	// Well-formed prelude announcing 64 total bytes, but only a few follow.
	body := []byte{
		0, 0, 0, 64, // totalLength
		0, 0, 0, 0, // headersLength
		0, 0, 0, 0, // prelude CRC
		1, 2, 3, // truncated payload
	}
	err := parseEventStream(bytes.NewReader(body), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("a truncated frame must report an error")
	}
	if errors.Is(err, errKiroEmptyStream) {
		t.Fatalf("a truncated frame must surface as a read error, not as the "+
			"empty-stream sentinel (got %v)", err)
	}
}

// Positive control 3: the guard must count a frame that the parser SKIPS as
// evidence the upstream responded. An undersized frame is malformed, but it still
// proves bytes arrived, and the pre-existing bounds test
// (TestEventStreamHandlesUndersizedFrameLength) pins that such a body parses
// without panicking.
func TestKiroUndersizedFrameIsNotAnEmptyStream(t *testing.T) {
	// totalLength = 8 (below the 16-byte floor) -> skipped, then EOF.
	prelude := []byte{
		0, 0, 0, 8, // totalLength < 16
		0, 0, 0, 0, // headersLength
		0, 0, 0, 0, // prelude CRC
	}
	err := parseEventStream(strings.NewReader(string(prelude)), &KiroStreamCallback{})
	if errors.Is(err, errKiroEmptyStream) {
		t.Fatal("a prelude that arrived and was skipped is not an empty stream: " +
			"the upstream did respond, so this must not be reported as no events")
	}
	if err != nil {
		t.Fatalf("an undersized frame must be skipped cleanly, got %v", err)
	}
}
