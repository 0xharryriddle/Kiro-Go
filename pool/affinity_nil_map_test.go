package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

// An AccountPool is legitimately constructible without every optional map
// populated — GetPool() initialises them all, but Reload()/selection are also
// driven on pools built field-by-field (see newTestPool), and a future field
// added to GetPool() but forgotten elsewhere would land in the same shape.
//
// A write to a nil Go map PANICS. So the affinity bind at the end of
// GetNextForModelWithApiKey must not assume the map exists: with session
// affinity enabled and apiKeyAffinity nil, the first successful selection would
// take down the whole proxy process rather than degrade to no affinity.
//
// This pins the defensive initialisation. The panic is caught here as a test
// failure rather than being allowed to kill the test binary.
func TestAffinityBindDoesNotPanicOnNilMap(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled: %v", err)
	}
	t.Cleanup(func() { _ = config.SetSessionAffinityEnabled(false) })

	// newTestPool deliberately leaves apiKeyAffinity (and circuitState) nil.
	p := newTestPool(config.Account{ID: "a", Enabled: true})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("affinity bind panicked on a nil map: %v", r)
		}
	}()

	got := p.GetNextForModelWithApiKey("", nil, "some-key")
	if got == nil || got.ID != "a" {
		t.Fatalf("expected account \"a\" to be selected, got %#v", got)
	}
	// The binding should have been recorded, not silently dropped.
	if len(p.apiKeyAffinity) != 1 {
		t.Fatalf("expected the binding to be stored, got %d entries", len(p.apiKeyAffinity))
	}
}

// The same hazard on the affinity-hit path: a pool with a populated binding but
// a nil lastDispatchSeq must still commit a dispatch without panicking.
func TestAffinityHitDoesNotPanicOnNilDispatchMap(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled: %v", err)
	}
	t.Cleanup(func() { _ = config.SetSessionAffinityEnabled(false) })

	if err := config.AddAccount(config.Account{ID: "bound", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.apiKeyAffinity = make(map[string]apiKeyBinding)
	p.Reload()
	p.apiKeyAffinity["key"] = apiKeyBinding{accountID: "bound", lastUsed: time.Now()}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("affinity hit panicked: %v", r)
		}
	}()

	got := p.GetNextForModelWithApiKey("", nil, "key")
	if got == nil || got.ID != "bound" {
		t.Fatalf("expected the bound account, got %#v", got)
	}
}
