package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

// DisableAccount and MarkOverLimit must leave a cooldown in place for BOTH
// shapes of id, and must never expose the account in between.
//
// Two orderings were tried and both were wrong on their own:
//
//	stamp -> Reload : no exposure window, but Reload's prune drops the cooldown
//	                  when the id is absent from config.
//	Reload -> stamp : survives the prune, but between Reload returning and the
//	                  stamp landing the account has no cooldown AND is present
//	                  in p.accounts, so a concurrent request selects the very
//	                  account we are disabling. Proven selectable.
//
// The guard is stamped BEFORE Reload (so there is never an unguarded instant)
// and again AFTER it (so the prune cannot erase it). setCooldownIfLater makes
// the second stamp idempotent — it can only extend, never shorten.
func TestDisableAccountCooldownForConfiguredAccount(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "bad", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "good", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.lastDispatchSeq = make(map[string]uint64)
	p.Reload()
	p.DisableAccount("bad", "revoked")

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["bad"]
	inPool := p.hasAccountLocked("bad")
	p.mu.RUnlock()

	if !hasCooldown {
		t.Fatal("DisableAccount left no cooldown for a configured account")
	}
	if inPool {
		t.Fatal("DisableAccount left the account in the routable set")
	}
	for i := 0; i < 5; i++ {
		if got := p.GetNextForModelExcluding("", nil); got == nil || got.ID == "bad" {
			t.Fatalf("iteration %d selected the disabled account: %#v", i, got)
		}
	}
}

// The id-absent-from-config case: the safety net must still hold, which is what
// the Reload-first ordering was introduced for.
func TestDisableAccountCooldownForConfigAbsentID(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("never-configured", "revoked")

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["never-configured"]
	p.mu.RUnlock()
	if !hasCooldown {
		t.Fatal("DisableAccount cooldown was pruned away for a config-absent id")
	}
}

// MarkOverLimit is the case where the window actually mattered: an over-quota
// account with upstream overage ENABLED stays in the routable set after Reload,
// so an unguarded instant is genuinely selectable. After the full call the
// cooldown must be in force.
func TestMarkOverLimitLeavesCooldownForRoutableAccount(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "hot", Enabled: true, UsageLimit: 100, UsageCurrent: 100,
		OverageStatus: "ENABLED",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.lastDispatchSeq = make(map[string]uint64)
	p.Reload()
	p.MarkOverLimit("hot")

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["hot"]
	inPool := p.hasAccountLocked("hot")
	p.mu.RUnlock()

	if !inPool {
		t.Fatal("setup invalid: overage-enabled account should stay in the pool")
	}
	if !hasCooldown {
		t.Fatal("MarkOverLimit left no cooldown, so the 402'd account is immediately reselectable")
	}
}

// A transient MarkOverLimit stamp must never shorten a longer quota backoff that
// is already in force — the second stamp is applied with setCooldownIfLater
// precisely so re-stamping cannot weaken an existing guard.
func TestMarkOverLimitDoesNotShortenLongerCooldown(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "hot", Enabled: true, OverageStatus: "ENABLED",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.Reload()

	long := time.Now().Add(4 * time.Hour)
	p.mu.Lock()
	p.cooldowns["hot"] = long
	p.mu.Unlock()

	p.MarkOverLimit("hot")

	p.mu.RLock()
	got := p.cooldowns["hot"]
	p.mu.RUnlock()
	if got.Before(long) {
		t.Fatalf("MarkOverLimit shortened an existing longer cooldown: %v < %v", got, long)
	}
}
