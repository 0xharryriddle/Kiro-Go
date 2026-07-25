package proxy

import (
	"encoding/json"
	"testing"
)

// The advertised model listing must state the input context window.
//
// It previously carried no window field at all. A client asking /v1/models
// therefore learned nothing about Opus 5's 1M window and fell back to its own
// built-in default, compacting the conversation while upstream still reported
// only ~8% context usage. Publishing the window is what lets a client actually
// fill the model.
func TestModelListingAdvertisesContextWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	info := buildModelInfo("claude-opus-5", "anthropic", true)

	// Every spelling clients are known to read must be present and agree.
	for _, key := range []string{"context_window", "context_length", "max_input_tokens"} {
		raw, ok := info[key]
		if !ok {
			t.Fatalf("model listing omits %q — clients cannot discover the window", key)
		}
		got, ok := raw.(int)
		if !ok {
			t.Fatalf("%s = %T, want int", key, raw)
		}
		if got != 1_000_000 {
			t.Errorf("%s = %d, want 1000000 for a flagship model", key, got)
		}
	}
}

// A 200K model must advertise 200K, not the flagship number. Over-reporting
// would make a client overflow the request upstream.
func TestModelListingAdvertisesSmallWindowForSmallModel(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	info := buildModelInfo("claude-sonnet-4.5", "anthropic", true)
	got, ok := info["context_window"].(int)
	if !ok {
		t.Fatalf("context_window missing or not an int: %v", info["context_window"])
	}
	if got != 200_000 {
		t.Fatalf("context_window = %d, want 200000", got)
	}
}

// An upstream-declared limit must win over the name heuristic in the listing,
// so a newly published model is advertised correctly with no code change.
func TestModelListingPrefersDeclaredLimit(t *testing.T) {
	resetDeclaredModelLimitsForTest()
	t.Cleanup(resetDeclaredModelLimitsForTest)

	recordModelTokenLimits([]ModelInfo{{
		ModelId: "claude-opus-9",
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 2_000_000, MaxOutputTokens: 128_000},
	}})

	info := buildModelInfo("claude-opus-9", "anthropic", true)
	got, ok := info["context_window"].(int)
	if !ok {
		t.Fatalf("context_window missing or not an int: %v", info["context_window"])
	}
	if got != 2_000_000 {
		t.Fatalf("context_window = %d, want the declared 2000000", got)
	}
	if got, ok := info["max_output_tokens"].(int); !ok || got != 128_000 {
		t.Fatalf("max_output_tokens = %v, want the declared 128000", info["max_output_tokens"])
	}
}

// An output ceiling must only appear when upstream actually declared one.
// Publishing a guess would make a client cap max_tokens below the real limit.
func TestModelListingOmitsOutputLimitWhenUndeclared(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	info := buildModelInfo("claude-opus-5", "anthropic", true)
	if _, present := info["max_output_tokens"]; present {
		t.Fatalf("max_output_tokens advertised without an upstream declaration")
	}
}

// The window must survive JSON serialization — a client reads the wire bytes,
// not the Go map.
func TestAdvertisedWindowSurvivesSerialization(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	raw, err := json.Marshal(buildModelInfo("claude-opus-5", "anthropic", true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := decoded["context_window"].(float64)
	if !ok {
		t.Fatalf("context_window missing after serialization: %s", raw)
	}
	if int(got) != 1_000_000 {
		t.Fatalf("serialized context_window = %d, want 1000000", int(got))
	}
}

// The thinking variant is a proxy-side routing name for the same underlying
// model, so it must advertise the same window. A client that picks the
// -thinking variant otherwise gets a different (smaller) window for no reason.
func TestThinkingVariantAdvertisesSameWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	base, baseOK := buildModelInfo("claude-opus-5", "anthropic", true)["context_window"].(int)
	thinking, thinkingOK := buildModelInfo("claude-opus-5-thinking", "anthropic", true)["context_window"].(int)
	if !baseOK || !thinkingOK {
		t.Fatalf("context_window missing: base=%v thinking=%v", baseOK, thinkingOK)
	}
	if base != thinking {
		t.Fatalf("thinking variant window %d != base window %d", thinking, base)
	}
}
