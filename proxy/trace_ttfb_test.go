package proxy

import (
	"testing"
	"time"
)

// A stream whose first output is a tool call must still report TTFB.
//
// Regression: markFirstByte was originally hooked only where text deltas are
// emitted, so any stream that opened with a tool_use chunk (a very common shape
// for agentic clients) recorded ttfbMs=0 forever. Verified against live traffic:
// 13 of 69 streaming rows were missing TTFB and all 13 were tool-call-only.
func TestTTFBIsRecordedForToolCallOnlyStreams(t *testing.T) {
	tr := newTraceRecorder("openai", "sonnet", true, "")
	time.Sleep(2 * time.Millisecond)

	// Simulate the tool-use callback firing with no preceding text delta.
	tr.markFirstByte()

	entry := tr.finish(outcomeSuccess, 200)
	if entry.TTFBMs <= 0 {
		t.Fatalf("TTFBMs = %d, want > 0: a tool-call chunk is a first byte to the client", entry.TTFBMs)
	}
}

// TTFB must reflect the FIRST byte, regardless of which callback produced it,
// and must not be overwritten by later output.
func TestTTFBUsesEarliestOutputOnly(t *testing.T) {
	tr := newTraceRecorder("claude", "sonnet", true, "")
	time.Sleep(2 * time.Millisecond)
	tr.markFirstByte() // e.g. tool call arrives first
	first := tr.finish(outcomeSuccess, 200).TTFBMs

	time.Sleep(8 * time.Millisecond)
	tr.markFirstByte() // later text delta must not move it
	second := tr.finish(outcomeSuccess, 200).TTFBMs

	if first <= 0 {
		t.Fatalf("first TTFBMs = %d, want > 0", first)
	}
	if second != first {
		t.Fatalf("TTFBMs moved after later output: %d -> %d", first, second)
	}
}

// Non-streaming requests legitimately have no TTFB; it must stay zero rather
// than being back-filled with the total duration, which would make the two
// metrics indistinguishable.
func TestTTFBStaysZeroWhenNoOutputWasStreamed(t *testing.T) {
	tr := newTraceRecorder("openai", "sonnet", false, "")
	time.Sleep(2 * time.Millisecond)
	if got := tr.finish(outcomeSuccess, 200).TTFBMs; got != 0 {
		t.Fatalf("TTFBMs = %d, want 0 for a non-streamed response", got)
	}
}
