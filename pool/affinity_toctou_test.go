package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

// The session-affinity fast path reads its eligibility inputs in several
// separate critical sections and then commits under a fresh write lock. Between
// the last read and the commit, a concurrent Reload can remove the bound account
// from the pool — and the path still returned the stale copy it captured
// earlier, dispatching one more request on a credential the completed reload was
// supposed to take out of rotation.
//
// The pause is applied by holding the account's circuit-breaker mutex: the
// affinity path calls isCircuitOpen after its account/cooldown/model/quota
// reads, so blocking there parks it in exactly the window under test without
// adding any test-only hook to production code. There is no data race here (the
// mutexes are correct); the defect is a stale routing decision, so -race cannot
// find it and an ordering test is the only instrument that can.
func TestAffinityDoesNotReturnAccountRemovedByReload(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled: %v", err)
	}
	t.Cleanup(func() { _ = config.SetSessionAffinityEnabled(false) })

	// Two accounts so normal selection still has somewhere to go once the bound
	// one is removed: this test must prove "not the removed account", not merely
	// "nil".
	for _, acc := range []config.Account{
		{ID: "bound", Enabled: true},
		{ID: "other", Enabled: true},
	} {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}

	p := healthPool()
	p.Reload()

	cb := &circuitBreaker{} // closed: eligibility passes, but isOpen must take cb.mu
	p.mu.Lock()
	p.circuitState["bound"] = cb
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}
	p.mu.Unlock()

	// Park the affinity path at its circuit check.
	cb.mu.Lock()
	result := make(chan *config.Account, 1)
	go func() { result <- p.GetNextForModelWithApiKey("", nil, "key") }()
	time.Sleep(50 * time.Millisecond)

	// Complete the removal while the affinity path is parked.
	if err := config.DeleteAccount("bound"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	p.Reload()
	cb.mu.Unlock()

	got := <-result
	if got == nil {
		t.Fatal("expected affinity to fall through to normal selection, got nil")
	}
	if got.ID == "bound" {
		t.Fatalf("affinity returned account %q after Reload removed it from the pool", got.ID)
	}
	if got.ID != "other" {
		t.Fatalf("expected fallthrough to the surviving account, got %q", got.ID)
	}
}

// The re-validation added for the case above must not break the ordinary
// affinity hit: a bound account that is still present has to keep being
// preferred, or session stickiness silently stops working.
func TestAffinityStillPrefersBoundAccountThatSurvivesReload(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled: %v", err)
	}
	t.Cleanup(func() { _ = config.SetSessionAffinityEnabled(false) })

	for _, acc := range []config.Account{
		{ID: "bound", Enabled: true},
		{ID: "other", Enabled: true},
	} {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}

	p := healthPool()
	p.Reload()
	p.mu.Lock()
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}
	p.mu.Unlock()

	// A Reload that removes nothing must leave the binding usable.
	p.Reload()

	for i := 0; i < 3; i++ {
		got := p.GetNextForModelWithApiKey("", nil, "key")
		if got == nil || got.ID != "bound" {
			t.Fatalf("affinity hit %d returned %#v, want the bound account", i+1, got)
		}
	}
}
