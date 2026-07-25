package proxy

import (
	"encoding/binary"
	"runtime"
	"strings"
	"testing"
)

// parseEventStream reads a 4-byte big-endian total length from each frame
// prelude and immediately does make([]byte, totalLength-12). That length is
// attacker/corruption controlled: a single 12-byte prelude claiming ~2GiB makes
// the proxy allocate ~2GiB before io.ReadFull can fail. On a small container
// that is an OOM kill from one malformed upstream frame.
//
// Real Kiro frames are far below maxEventFrameBytes, so bounding the allocation
// cannot reject legitimate traffic.
func TestEventStreamRejectsOversizedFrameLength(t *testing.T) {
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], 0x0C000000) // 192 MiB
	binary.BigEndian.PutUint32(prelude[4:8], 0)
	body := append(prelude, 0x01, 0x02, 0x03) // truncated payload

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := parseEventStream(strings.NewReader(string(body)), &KiroStreamCallback{})
	runtime.ReadMemStats(&after)

	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("err=%v allocated=%d bytes", err, alloc)

	// Must not honour the bogus length. 16 MiB of headroom is generous: the
	// unbounded version allocated ~192 MiB here.
	if alloc > 16<<20 {
		t.Fatalf("allocated %d bytes from an unvalidated 4-byte length field", alloc)
	}
	if err == nil {
		t.Fatal("expected an error for an oversized frame length")
	}
}

// A frame length below the 16-byte floor must not panic or allocate wildly.
func TestEventStreamHandlesUndersizedFrameLength(t *testing.T) {
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], 8) // < 16
	binary.BigEndian.PutUint32(prelude[4:8], 0)

	// No panic, no hang: the frame is skipped and the stream ends cleanly.
	if err := parseEventStream(strings.NewReader(string(prelude)), &KiroStreamCallback{}); err != nil {
		t.Fatalf("undersized frame should be skipped, got err=%v", err)
	}
}

// Guard against over-correction: a normal, well-formed frame must still parse.
// This is the regression that would catch a bound set too aggressively low.
// It reuses awsEventStreamFrame (kiro_test.go), the existing helper that builds
// real AWS event-stream frames.
func TestEventStreamStillParsesNormalFrame(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	})

	var got string
	err := parseEventStream(strings.NewReader(string(frame)), &KiroStreamCallback{
		OnText: func(text string, isThinking bool) { got += text },
	})
	if err != nil {
		t.Fatalf("well-formed frame failed to parse: %v", err)
	}
	if got != "hello" {
		t.Fatalf("expected the frame payload to be delivered, got %q", got)
	}
}
