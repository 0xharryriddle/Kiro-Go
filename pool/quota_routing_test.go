package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
)

// enableQuotaAwareForTest initializes a fresh config in a temp dir and turns on
// quota-aware routing, restoring the toggle afterward so sibling tests that rely
// on round-robin are unaffected.
func enableQuotaAwareForTest(t *testing.T) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateQuotaAwareRouting(true); err != nil {
		t.Fatalf("UpdateQuotaAwareRouting: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateQuotaAwareRouting(false) })
}

func TestQuotaAwareRoutingPicksMostRemainingQuota(t *testing.T) {
	enableQuotaAwareForTest(t)
	// b has the most remaining quota (100-10=90) vs a (100-90=10), c (50-40=10).
	p := newTestPool(
		config.Account{ID: "a", UsageCurrent: 90, UsageLimit: 100},
		config.Account{ID: "b", UsageCurrent: 10, UsageLimit: 100},
		config.Account{ID: "c", UsageCurrent: 40, UsageLimit: 50},
	)
	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account on iteration %d", i)
		}
		if acc.ID != "b" {
			t.Fatalf("expected highest-remaining-quota account b, got %q", acc.ID)
		}
	}
}

func TestQuotaAwareRoutingSkipsExcludedAndCooldown(t *testing.T) {
	enableQuotaAwareForTest(t)
	p := newTestPool(
		config.Account{ID: "a", UsageCurrent: 0, UsageLimit: 100}, // most quota but excluded
		config.Account{ID: "b", UsageCurrent: 50, UsageLimit: 100},
	)
	excluded := map[string]bool{"a": true}
	acc := p.GetNextExcluding(excluded)
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected b when a excluded, got %#v", acc)
	}
}

func TestQuotaAwareRoutingFallsBackToRoundRobinWhenNoQuotaData(t *testing.T) {
	enableQuotaAwareForTest(t)
	// Neither account has UsageLimit data, so quota-aware yields nothing and the
	// round-robin fallback must still return accounts.
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected round-robin fallback to return an account on iteration %d", i)
		}
		seen[acc.ID]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("expected round-robin to rotate across both accounts, got %v", seen)
	}
}

func TestQuotaAwareRoutingRespectsModelCapability(t *testing.T) {
	enableQuotaAwareForTest(t)
	// a has more quota but does NOT serve the model; b does.
	p := newTestPool(
		config.Account{ID: "a", UsageCurrent: 0, UsageLimit: 100},
		config.Account{ID: "b", UsageCurrent: 80, UsageLimit: 100},
	)
	p.SetModelList("a", []string{"other-model"})
	p.SetModelList("b", []string{"target-model"})
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModel("target-model")
		if acc == nil {
			t.Fatalf("expected a capable account on iteration %d", i)
		}
		if acc.ID != "b" {
			t.Fatalf("expected model-capable account b, got %q", acc.ID)
		}
	}
}

func TestQuotaAwareRoutingSkipsQuotaBlocked(t *testing.T) {
	enableQuotaAwareForTest(t)
	// a is over quota (blocked, no overage), b has quota left.
	p := newTestPool(
		config.Account{ID: "a", UsageCurrent: 100, UsageLimit: 100},
		config.Account{ID: "b", UsageCurrent: 30, UsageLimit: 100},
	)
	acc := p.GetNext()
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected non-blocked account b, got %#v", acc)
	}
}

func TestRemainingQuota(t *testing.T) {
	if rem, ok := remainingQuota(config.Account{UsageCurrent: 30, UsageLimit: 100}); !ok || rem != 70 {
		t.Fatalf("expected (70,true), got (%v,%v)", rem, ok)
	}
	if rem, ok := remainingQuota(config.Account{UsageCurrent: 120, UsageLimit: 100}); !ok || rem != 0 {
		t.Fatalf("expected clamp to (0,true), got (%v,%v)", rem, ok)
	}
	if _, ok := remainingQuota(config.Account{UsageCurrent: 5}); ok {
		t.Fatalf("expected ok=false when UsageLimit is absent")
	}
}
