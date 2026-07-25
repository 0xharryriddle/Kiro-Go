package proxy

import "testing"

// The window this proxy ADVERTISES must never exceed the window it will actually
// carry. This is the invariant that ties /v1/models to truncatePayloadToLimit.
//
// It was violated for the sonnet and haiku families: the listing published
// getContextWindowSize (nominal, 1M for any >= 4.6 name) while dispatch enforced
// truncationContextWindow (200K, because that is what Kiro serves those families
// behind). A client told sonnet-4.6 held 1M would fill the conversation to 1M and
// this proxy would silently discard ~80% of it before sending — the user loses
// context and is never told.
//
// Under-advertising is acceptable (the client compacts a little early).
// Over-advertising is not (the client loses context it believes it still has).
func TestAdvertisedWindowNeverExceedsCarriedWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	models := []string{
		// Opus line: genuinely large-context.
		"claude-opus-5", "claude-opus-5-thinking", "claude-opus-5.1",
		"claude-opus-6", "claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6",
		"claude-opus-4.5",
		// Sonnet/haiku: pinned to 200K by Kiro regardless of version. These are
		// the names that regressed.
		"claude-sonnet-4.6", "claude-sonnet-5", "claude-sonnet-5-thinking",
		"claude-haiku-5", "claude-sonnet-4.5", "claude-sonnet-4", "claude-haiku-4.5",
		// Unknown / non-Claude identifiers.
		"unknown-model", "",
	}

	for _, model := range models {
		info := buildModelInfo(model, "anthropic", true)
		advertised, ok := info["context_window"].(int)
		if !ok {
			t.Errorf("%q: context_window missing from listing", model)
			continue
		}
		carried := truncationContextWindow(model)
		if advertised > carried {
			t.Errorf("%q: advertises %d but only carries %d — clients will lose %d tokens silently",
				model, advertised, carried, advertised-carried)
		}
	}
}

// Every advertised spelling must agree. A client reading context_length while
// another reads max_input_tokens must not get different answers.
func TestAdvertisedWindowSpellingsAgree(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	for _, model := range []string{"claude-opus-5", "claude-sonnet-4.6", "claude-haiku-4.5"} {
		info := buildModelInfo(model, "anthropic", true)
		want, ok := info["context_window"].(int)
		if !ok {
			t.Fatalf("%q: context_window missing", model)
		}
		for _, key := range []string{"context_length", "max_input_tokens"} {
			got, ok := info[key].(int)
			if !ok {
				t.Errorf("%q: %s missing", model, key)
				continue
			}
			if got != want {
				t.Errorf("%q: %s = %d disagrees with context_window = %d", model, key, got, want)
			}
		}
	}
}

// The opus line must still advertise its full 1M window — the consistency fix
// must not have flattened everything to the conservative 200K floor.
func TestOpusStillAdvertisesFullWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	for _, model := range []string{"claude-opus-5", "claude-opus-5-thinking", "claude-opus-4.6"} {
		info := buildModelInfo(model, "anthropic", true)
		got, ok := info["context_window"].(int)
		if !ok {
			t.Fatalf("%q: context_window missing", model)
		}
		if got != 1_000_000 {
			t.Errorf("%q advertises %d, want the full 1000000", model, got)
		}
	}
}

// An upstream-declared limit must lift the sonnet/haiku pin in BOTH the listing
// and the dispatch budget, so the two stay consistent when Kiro tells us the
// family pin no longer applies.
func TestDeclaredLimitLiftsPinConsistently(t *testing.T) {
	resetDeclaredModelLimitsForTest()
	t.Cleanup(resetDeclaredModelLimitsForTest)

	// Precondition: the name alone is pinned to 200K.
	if got := truncationContextWindow("claude-sonnet-4.6"); got != 200_000 {
		t.Fatalf("precondition: sonnet carried window = %d, want the 200000 pin", got)
	}

	recordModelTokenLimits([]ModelInfo{{
		ModelId: "claude-sonnet-4.6",
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 1_000_000},
	}})

	carried := truncationContextWindow("claude-sonnet-4.6")
	if carried != 1_000_000 {
		t.Fatalf("declared limit did not lift the pin for dispatch: carried = %d", carried)
	}
	info := buildModelInfo("claude-sonnet-4.6", "anthropic", true)
	advertised, ok := info["context_window"].(int)
	if !ok {
		t.Fatal("context_window missing")
	}
	if advertised != carried {
		t.Fatalf("declared limit lifted dispatch to %d but the listing still says %d", carried, advertised)
	}
}
