package pool

import (
	"kiro-go/config"
	"testing"
)

func TestModelMatrixEmptyWhenNoCache(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})
	entries, withCache := p.ModelMatrix()
	if withCache != 0 {
		t.Fatalf("expected 0 accounts with cache, got %d", withCache)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty matrix, got %d entries", len(entries))
	}
}

func TestModelMatrixUnionAndCapableAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
		config.Account{ID: "c"},
	)
	// a and b both serve shared-model; only a serves a-only; c serves c-only.
	p.SetModelList("a", []string{"shared-model", "a-only"})
	p.SetModelList("b", []string{"shared-model"})
	p.SetModelList("c", []string{"c-only"})

	entries, withCache := p.ModelMatrix()
	if withCache != 3 {
		t.Fatalf("expected 3 accounts with cache, got %d", withCache)
	}
	// 3 distinct models: a-only, c-only, shared-model (sorted).
	if len(entries) != 3 {
		t.Fatalf("expected 3 models, got %d: %#v", len(entries), entries)
	}

	byModel := map[string]ModelMatrixEntry{}
	for _, e := range entries {
		byModel[e.Model] = e
	}

	shared, ok := byModel["shared-model"]
	if !ok {
		t.Fatal("expected shared-model entry")
	}
	if shared.CapableCount != 2 {
		t.Fatalf("expected shared-model served by 2 accounts, got %d", shared.CapableCount)
	}
	// Account IDs must be sorted for stable UI ordering.
	if len(shared.AccountIDs) != 2 || shared.AccountIDs[0] != "a" || shared.AccountIDs[1] != "b" {
		t.Fatalf("expected sorted [a b], got %v", shared.AccountIDs)
	}

	if byModel["a-only"].CapableCount != 1 || byModel["a-only"].AccountIDs[0] != "a" {
		t.Fatalf("expected a-only served only by a, got %#v", byModel["a-only"])
	}
	if byModel["c-only"].CapableCount != 1 || byModel["c-only"].AccountIDs[0] != "c" {
		t.Fatalf("expected c-only served only by c, got %#v", byModel["c-only"])
	}

	// Entries must be sorted by model name: a-only, c-only, shared-model.
	if entries[0].Model != "a-only" || entries[1].Model != "c-only" || entries[2].Model != "shared-model" {
		t.Fatalf("expected models sorted, got %s %s %s", entries[0].Model, entries[1].Model, entries[2].Model)
	}
}
