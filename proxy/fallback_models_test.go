package proxy

import (
	"strings"
	"testing"
)

// The fallback list is what clients see when the upstream model list is
// unavailable (no enabled account, or every ListAvailableModels probe failed).
//
// It previously topped out at opus-4.7 with no 5.x entry at all, so a degraded
// proxy advertised a fleet whose best model was two releases stale and a client
// picking from it could never select the current flagship.
func TestFallbackModelsIncludeCurrentFlagship(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	models := fallbackAnthropicModels("-thinking")
	if len(models) == 0 {
		t.Fatal("fallback list is empty")
	}

	ids := make(map[string]map[string]interface{}, len(models))
	for _, m := range models {
		id, _ := m["id"].(string)
		ids[id] = m
	}

	for _, want := range []string{"claude-opus-5", "claude-opus-5-thinking"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("fallback list omits %q", want)
		}
	}
}

// The flagship's fallback entry must advertise its real window, otherwise the
// degraded path reintroduces exactly the under-reporting this work fixed.
func TestFallbackFlagshipAdvertisesLargeWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	for _, m := range fallbackAnthropicModels("-thinking") {
		id, _ := m["id"].(string)
		if id != "claude-opus-5" {
			continue
		}
		got, ok := m["context_window"].(int)
		if !ok {
			t.Fatalf("claude-opus-5 fallback entry has no int context_window: %v", m["context_window"])
		}
		if got != 1_000_000 {
			t.Fatalf("claude-opus-5 fallback context_window = %d, want 1000000", got)
		}
		return
	}
	t.Fatal("claude-opus-5 not present in fallback list")
}

// Every fallback entry must carry a window, and it must never exceed what the
// proxy will actually forward for that model (same invariant as the live list).
func TestFallbackEntriesNeverOverPromiseWindow(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	for _, m := range fallbackAnthropicModels("-thinking") {
		id, _ := m["id"].(string)
		advertised, ok := m["context_window"].(int)
		if !ok {
			t.Errorf("%q: fallback entry missing context_window", id)
			continue
		}
		carried := truncationContextWindow(id)
		if advertised > carried {
			t.Errorf("%q: fallback advertises %d but only carries %d",
				id, advertised, carried)
		}
	}
}

// Thinking variants must be present for every base model, since that is how a
// client requests extended reasoning through this proxy.
func TestFallbackPairsEveryModelWithThinkingVariant(t *testing.T) {
	resetDeclaredModelLimitsForTest()

	const suffix = "-thinking"
	present := map[string]bool{}
	for _, m := range fallbackAnthropicModels(suffix) {
		id, _ := m["id"].(string)
		present[id] = true
	}

	for id := range present {
		if strings.HasSuffix(id, suffix) {
			continue
		}
		if !present[id+suffix] {
			t.Errorf("%q has no %s variant in the fallback list", id, suffix)
		}
	}
}
