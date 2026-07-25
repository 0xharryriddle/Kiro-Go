package proxy

import (
	"strings"
	"testing"
)

// buildShrinkFixture returns a payload with a long history and a system priming
// pair, sized so it comfortably fits its model's ceiling (so ordinary
// truncation has already run and left it alone).
func buildShrinkFixture(t *testing.T, model string, turns int) *KiroPayload {
	t.Helper()
	chunk := strings.Repeat("lorem ipsum dolor sit amet ", 40)

	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step: " + chunk},
			ClaudeMessage{Role: "user", Content: "next: " + chunk},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: answer"})

	return ClaudeToKiro(&ClaudeRequest{
		Model:    model,
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}, false)
}

// The recovery path for an over-estimated ceiling: when upstream rejects a
// payload as too long, it must actually get smaller. Without this the request
// fails outright, because rotating endpoints or accounts cannot help — every one
// of them rejects the identical bytes.
func TestShrinkAfterLengthRejectionReducesPayload(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	payload := buildShrinkFixture(t, "claude-opus-5", 60)
	beforeBytes := payloadByteSize(payload)
	beforeTokens := estimateKiroPayloadTokens(payload)

	if !shrinkPayloadAfterLengthRejection(payload) {
		t.Fatalf("shrink reported no reduction (before: %d bytes, %d tokens)", beforeBytes, beforeTokens)
	}

	afterBytes := payloadByteSize(payload)
	if afterBytes >= beforeBytes {
		t.Fatalf("payload did not shrink: %d -> %d bytes", beforeBytes, afterBytes)
	}
	// The cut targets ~half the rejected size, so expect a substantial drop
	// rather than a token trim of a few bytes.
	if afterBytes > beforeBytes*3/4 {
		t.Errorf("shrink was too timid: %d -> %d bytes (want <= 75%%)", beforeBytes, afterBytes)
	}
}

// Shrinking must preserve the invariants ordinary truncation guarantees: the
// current message is what the model is being asked to answer, so losing it turns
// a too-long request into a nonsensical one.
func TestShrinkPreservesCurrentMessage(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	payload := buildShrinkFixture(t, "claude-opus-5", 60)
	if !shrinkPayloadAfterLengthRejection(payload) {
		t.Fatal("expected a reduction")
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: answer") {
		got := cur.Content
		if len(got) > 120 {
			got = got[:120]
		}
		t.Fatalf("current message lost during shrink, got %q", got)
	}
	// The model ID must survive too — it drives every subsequent budget.
	if cur.ModelID == "" {
		t.Fatal("shrink cleared the current message's model ID")
	}
}

// An elision marker must be present so the model knows context was dropped
// rather than silently believing it has the whole conversation.
func TestShrinkMarksElidedHistory(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	payload := buildShrinkFixture(t, "claude-opus-5", 60)
	if !shrinkPayloadAfterLengthRejection(payload) {
		t.Fatal("expected a reduction")
	}

	found := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("shrink dropped history without inserting an elision placeholder")
	}
}

// Termination guard: a payload that cannot be reduced any further must report
// false, so the dispatch loop surfaces the error instead of retrying forever on
// an unshrinkable request.
func TestShrinkReportsFalseWhenNothingLeftToDrop(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	// Minimal payload: no history at all, one short current message.
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-opus-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}, false)

	// Repeated shrinks must converge to "cannot reduce" rather than looping.
	// Ten iterations is far more than the dispatch path ever performs (one).
	sawFalse := false
	for i := 0; i < 10; i++ {
		if !shrinkPayloadAfterLengthRejection(payload) {
			sawFalse = true
			break
		}
	}
	if !sawFalse {
		t.Fatal("shrink never reported false on a minimal payload — dispatch could retry unboundedly")
	}
}

// A nil payload must not panic: the dispatch path calls this on whatever it was
// handed after an upstream rejection.
func TestShrinkHandlesNilPayload(t *testing.T) {
	if shrinkPayloadAfterLengthRejection(nil) {
		t.Fatal("nil payload reported a successful reduction")
	}
}

// truncatePayloadToBudget with the model's own ceiling must behave exactly like
// truncatePayloadToLimit — the refactor that introduced the budget parameter
// must not have changed default behaviour.
func TestBudgetVariantMatchesDefaultBehaviour(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	a := buildShrinkFixture(t, "claude-sonnet-4.5", 200)
	b := buildShrinkFixture(t, "claude-sonnet-4.5", 200)

	truncatePayloadToLimit(a, true)
	truncatePayloadToBudget(b, true, maxPayloadTokens("claude-sonnet-4.5"))

	if payloadByteSize(a) != payloadByteSize(b) {
		t.Fatalf("budget variant diverged from default: %d vs %d bytes",
			payloadByteSize(a), payloadByteSize(b))
	}
	if len(a.ConversationState.History) != len(b.ConversationState.History) {
		t.Fatalf("budget variant retained a different history length: %d vs %d",
			len(a.ConversationState.History), len(b.ConversationState.History))
	}
}
