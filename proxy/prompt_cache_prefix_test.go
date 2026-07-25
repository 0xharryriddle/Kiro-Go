package proxy

import (
	"strings"
	"testing"
)

// bigText returns filler long enough to clear the Opus minimum cacheable
// token threshold (4096) so the tracker does not discard the breakpoint.
func bigText(seed string, repeat int) string {
	return strings.Repeat(seed+" lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore ", repeat)
}

func textBlock(text string) map[string]interface{} {
	return map[string]interface{}{"type": "text", "text": text}
}

func textBlockCached(text string) map[string]interface{} {
	return map[string]interface{}{
		"type":          "text",
		"text":          text,
		"cache_control": map[string]interface{}{"type": "ephemeral"},
	}
}

// TestMovingBreakpointStillMatchesStablePrefix reproduces the real Claude Code
// wire pattern: one cache_control marker stays pinned on the system prompt and
// a second marker MOVES to the newest user turn on every request.
//
// Turn 1: system[cached] + user0[cached]
// Turn 2: system[cached] + user0(marker removed) + user1[cached]
//
// The bytes of system and user0 are identical across both turns, so the prefix
// ending at user0 must be recognised as a cache hit on turn 2. If the marker
// itself is part of the fingerprint, user0 hashes differently once the marker
// moves off it, the chain diverges, and every turn pays full price.
func TestMovingBreakpointStillMatchesStablePrefix(t *testing.T) {
	tracker := newPromptCacheTracker(0)
	const account = "acct-moving-breakpoint"

	// Each block must independently clear the Opus 4096-token floor, otherwise
	// the tracker discards the breakpoint and the test would go red for the
	// wrong reason (threshold, not marker position).
	sys := bigText("system", 260)
	user0 := bigText("user-zero", 260)
	user1 := bigText("user-one", 20)

	turn1 := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: []interface{}{textBlockCached(sys)},
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{textBlockCached(user0)}},
		},
	}

	// Turn 2: same prefix bytes, but the moving marker has advanced to user1.
	turn2 := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: []interface{}{textBlockCached(sys)},
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{textBlock(user0)}},
			{Role: "assistant", Content: []interface{}{textBlock("ack")}},
			{Role: "user", Content: []interface{}{textBlockCached(user1)}},
		},
	}

	profile1 := tracker.BuildClaudeProfile(turn1, 0)
	if profile1 == nil {
		t.Fatal("turn 1 produced no cache profile")
	}
	tracker.Update(account, profile1)

	profile2 := tracker.BuildClaudeProfile(turn2, 0)
	if profile2 == nil {
		t.Fatal("turn 2 produced no cache profile")
	}

	usage := tracker.Compute(account, profile2)
	t.Logf("turn2 cacheRead=%d cacheCreate=%d totalInput=%d",
		usage.CacheReadInputTokens, usage.CacheCreationInputTokens, profile2.TotalInputTokens)

	if usage.CacheReadInputTokens <= 0 {
		t.Fatalf("stable system+user0 prefix was NOT reused after the marker moved: cacheRead=%d (want >0)", usage.CacheReadInputTokens)
	}
}

// TestStablePrefixFingerprintIgnoresMarkerPosition isolates the same defect at
// the fingerprint level: the prefix hash for an unchanged system block must not
// depend on whether a cache_control marker happens to sit on it this turn.
func TestStablePrefixFingerprintIgnoresMarkerPosition(t *testing.T) {
	sys := bigText("system", 60)

	withMarker := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: []interface{}{textBlockCached(sys)},
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{textBlockCached("hello")}},
		},
	}
	withoutMarker := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: []interface{}{textBlock(sys)},
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{textBlockCached("hello")}},
		},
	}

	a := flattenClaudeCacheBlocks(withMarker)
	b := flattenClaudeCacheBlocks(withoutMarker)
	if len(a) != len(b) {
		t.Fatalf("block count differs: %d vs %d", len(a), len(b))
	}

	// Block index 1 is the system block (index 0 is the request prelude).
	ca := canonicalizeCacheValue(a[1].Value)
	cb := canonicalizeCacheValue(b[1].Value)
	if ca != cb {
		t.Fatalf("system block fingerprint changed with marker position:\n with marker: %s\n without   : %s", ca, cb)
	}
}
