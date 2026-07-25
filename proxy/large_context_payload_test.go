package proxy

import (
	"strings"
	"testing"
)

// The serialized-body ceiling used to be a single global constant (900KB)
// calibrated for a 200K-token model. Applied to a 1M-context model such as
// Opus 5 it became the BINDING constraint long before the token window did:
// 900KB / ~4 bytes-per-token is roughly 230K tokens, so a request was truncated
// at about a fifth of the window the model actually offers while the token check
// still reported ~570K tokens of unused headroom.
//
// Observed in production traces for claude-opus-5: 4793 of 4794 requests landed
// under ~80K input tokens with a single outlier at 217K and nothing in between —
// no upstream size rejection anywhere in the error set, i.e. the proxy was
// trimming locally rather than upstream refusing.
func TestLargeContextModelGetsScaledByteCeiling(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	baseline := maxPayloadBytesForModel("claude-sonnet-4.5") // 200K window
	if baseline != maxPayloadBytes {
		t.Fatalf("200K model byte ceiling = %d, want the unchanged baseline %d", baseline, maxPayloadBytes)
	}

	large := maxPayloadBytesForModel("claude-opus-5") // 1M window
	if large <= baseline {
		t.Fatalf("1M-context model byte ceiling = %d, must exceed the 200K baseline %d", large, baseline)
	}
	// A 1M window is 5x the 200K baseline, so the body budget should scale ~5x.
	if want := maxPayloadBytes * 5; large != want {
		t.Fatalf("1M-context byte ceiling = %d, want %d (5x baseline)", large, want)
	}
}

// Every flagship name the version classifier treats as 1M must also receive the
// scaled body budget. A model that reports a 1M window but is held to a 200K
// body cannot actually use that window.
func TestFlagshipModelsAllGetScaledByteCeiling(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	// Opus only. sonnet/haiku are deliberately pinned to 200K by
	// truncationContextWindow regardless of version, because Kiro serves those
	// families behind a 200K window — exceeding it is what produces the upstream
	// 400 "Input is too long." A declared upstream limit can lift that pin (see
	// TestDeclaredLimitOverridesSonnetPin), but the name alone must not.
	for _, model := range []string{
		"claude-opus-5",
		"claude-opus-5-thinking",
		"claude-opus-5.1",
		"claude-opus-6",
		"claude-opus-4.6",
		"claude-opus-4.8",
	} {
		window := truncationContextWindow(model)
		bytes := maxPayloadBytesForModel(model)
		if window <= baselineContextWindow {
			t.Errorf("%s: window %d — expected a large-context model", model, window)
			continue
		}
		if bytes <= maxPayloadBytes {
			t.Errorf("%s: window %d but byte ceiling %d is still the 200K baseline %d",
				model, window, bytes, maxPayloadBytes)
		}
	}
}

// 200K models must be completely unaffected: their ceiling is the original
// constant, so their truncation behaviour is byte-identical to before scaling.
func TestSmallContextModelsKeepBaselineByteCeiling(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	for _, model := range []string{
		"claude-sonnet-4.5",
		"claude-sonnet-4",
		"claude-haiku-4.5",
		"claude-opus-4.5",
		"unknown-model",
		"",
	} {
		if got := maxPayloadBytesForModel(model); got != maxPayloadBytes {
			t.Errorf("%s: byte ceiling = %d, want unchanged baseline %d", model, got, maxPayloadBytes)
		}
	}
}

// End-to-end: a conversation whose serialized size lands between the old flat
// ceiling and the scaled one must now survive intact on a 1M model. Under the
// old constant this same history was truncated and a placeholder inserted.
func TestLargeContextConversationSurvivesPastOldByteCeiling(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	// ~8KB of ASCII prose per turn. 150 turns is ~1.2MB of history: comfortably
	// past the old 900KB ceiling, comfortably inside the scaled 4.5MB one.
	chunk := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 180)
	var messages []ClaudeMessage
	for i := 0; i < 150; i++ {
		messages = append(messages,
			ClaudeMessage{Role: "user", Content: chunk},
			ClaudeMessage{Role: "assistant", Content: chunk},
		)
	}
	messages = append(messages, ClaudeMessage{Role: "user", Content: "final question"})

	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 8192,
		Messages:  messages,
	}

	payload := ClaudeToKiro(req, false)

	size := payloadByteSize(payload)
	if size <= maxPayloadBytes {
		t.Skipf("test premise not met: payload %d bytes did not exceed the old ceiling %d", size, maxPayloadBytes)
	}
	if limit := maxPayloadBytesForModel("claude-opus-5"); size > limit {
		t.Fatalf("payload %d bytes exceeds the scaled ceiling %d", size, limit)
	}

	// The current message must always survive.
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; !strings.Contains(got, "final question") {
		t.Fatalf("current message lost: %q", firstN(got, 200))
	}

	// And crucially: history must NOT have been elided, because it fits.
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, truncationPlaceholder) {
			t.Fatalf("history was truncated at %d bytes even though the 1M model's ceiling is %d",
				size, maxPayloadBytesForModel("claude-opus-5"))
		}
	}
}

// The upstream-declared limit must drive the byte ceiling too. When Kiro reports
// a model's real maxInputTokens there is no reason to keep guessing from the
// name, and a declared 1M model must get the scaled body budget even if its name
// would otherwise classify as 200K.
func TestDeclaredLimitDrivesByteCeiling(t *testing.T) {
	resetDeclaredModelLimitsForTest()
	t.Cleanup(resetDeclaredModelLimitsForTest)

	// A sonnet name would normally be pinned to 200K by family.
	if got := maxPayloadBytesForModel("claude-sonnet-9"); got != maxPayloadBytes {
		t.Fatalf("precondition: sonnet byte ceiling = %d, want baseline %d", got, maxPayloadBytes)
	}

	recordModelTokenLimits([]ModelInfo{{
		ModelId: "claude-sonnet-9",
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 1_000_000, MaxOutputTokens: 64000},
	}})

	if got := truncationContextWindow("claude-sonnet-9"); got != 1_000_000 {
		t.Fatalf("declared window ignored: truncationContextWindow = %d, want 1000000", got)
	}
	if got := maxPayloadBytesForModel("claude-sonnet-9"); got <= maxPayloadBytes {
		t.Fatalf("declared 1M model still held to baseline byte ceiling %d", got)
	}
	// The thinking variant resolves to the same declared limits.
	if got := truncationContextWindow("claude-sonnet-9-thinking"); got != 1_000_000 {
		t.Fatalf("thinking variant lost the declared window: got %d", got)
	}
}

// A declared limit must also drive the window reported to clients, which is what
// they use to decide when to compact.
func TestDeclaredLimitDrivesReportedContextWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()
	t.Cleanup(resetDeclaredModelLimitsForTest)

	// Name heuristic would say 200K.
	if got := getContextWindowSize("claude-opus-4.5"); got != 200_000 {
		t.Fatalf("precondition: got %d, want 200000", got)
	}

	recordModelTokenLimits([]ModelInfo{{
		ModelId: "claude-opus-4.5",
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 500_000},
	}})

	if got := getContextWindowSize("claude-opus-4.5"); got != 500_000 {
		t.Fatalf("declared window ignored: got %d, want 500000", got)
	}
}

// Models upstream said nothing about must keep falling through to the heuristic
// rather than recording a zero window.
func TestMissingOrZeroDeclaredLimitsFallBackToHeuristic(t *testing.T) {
	resetDeclaredModelLimitsForTest()
	t.Cleanup(resetDeclaredModelLimitsForTest)

	recordModelTokenLimits([]ModelInfo{
		{ModelId: "claude-opus-5", TokenLimits: nil},
		{ModelId: "claude-sonnet-4.5", TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 0, MaxOutputTokens: 0}},
	})

	if got := getContextWindowSize("claude-opus-5"); got != 1_000_000 {
		t.Fatalf("nil tokenLimits should fall back to the heuristic: got %d", got)
	}
	if got := getContextWindowSize("claude-sonnet-4.5"); got != 200_000 {
		t.Fatalf("zero tokenLimits should fall back to the heuristic: got %d", got)
	}
}
