package proxy

// TEMPORARY self-audit. Deleted before finishing.
//
// The one question that matters about the exception-frame change: can it drop a
// frame that works today? The branch fires only on `:message-type == "exception"`,
// so a normal frame must survive every other shape of that header — present with
// value "event", absent entirely, or present but empty.

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func writeFrameWithMessageType(buf *bytes.Buffer, eventType, msgType, payload string) {
	var headers bytes.Buffer
	if msgType != "" {
		writeStringHeader(&headers, ":message-type", msgType)
	}
	writeStringHeader(&headers, ":event-type", eventType)

	body := []byte(payload)
	hb := headers.Bytes()
	total := 12 + len(hb) + len(body) + 4
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(hb)))
	buf.Write(prelude)
	buf.Write(hb)
	buf.Write(body)
	buf.Write([]byte{0, 0, 0, 0})
}

func TestNormalFramesSurviveExceptionDetection(t *testing.T) {
	cases := []struct {
		name    string
		msgType string
	}{
		{"message-type=event (the real normal case)", "event"},
		{"message-type absent entirely", ""},
		{"message-type present but empty", " "},
		{"message-type=Exception wrong case", "Exception"},
		{"message-type=exception_something", "exception_something"},
	}
	for _, c := range cases {
		var stream bytes.Buffer
		writeFrameWithMessageType(&stream, "assistantResponseEvent", c.msgType, `{"content":"KEEPME"}`)

		var got string
		cb := KiroStreamCallback{OnText: func(s string, _ bool) { got += s }}
		if err := parseEventStream(&stream, &cb); err != nil {
			t.Errorf("%s: parseEventStream error: %v", c.name, err)
			continue
		}
		if got != "KEEPME" {
			t.Errorf("CONTENT LOST for %s: got %q, want %q", c.name, got, "KEEPME")
		} else {
			t.Logf("ok  %-42s content preserved", c.name)
		}
	}
}

// Adversarial/truncated headers must not panic.
func TestAdversarialHeadersDoNotPanic(t *testing.T) {
	bad := [][]byte{
		{},
		{5},
		{5, 'a'},
		{13, ':', 'm', 'e', 's', 's', 'a', 'g', 'e', '-', 't', 'y', 'p', 'e'},
		{13, ':', 'm', 'e', 's', 's', 'a', 'g', 'e', '-', 't', 'y', 'p', 'e', 7},
		{13, ':', 'm', 'e', 's', 's', 'a', 'g', 'e', '-', 't', 'y', 'p', 'e', 7, 0},
		{13, ':', 'm', 'e', 's', 's', 'a', 'g', 'e', '-', 't', 'y', 'p', 'e', 7, 0xFF, 0xFF},
		{1, 'x', 99},
		{1, 'x', 6, 0xFF, 0xFF},
	}
	for i, h := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PANIC on adversarial header set %d: %v", i, r)
				}
			}()
			_ = extractHeaderString(h, ":message-type")
			_ = extractHeaderString(h, ":exception-type")
			_ = extractEventType(h)
		}()
	}
	t.Logf("all %d adversarial header sets handled without panic", len(bad))
}
