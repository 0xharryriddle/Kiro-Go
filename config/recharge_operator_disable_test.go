package config

import (
	"path/filepath"
	"testing"
)

// A recharge must not resurrect a key an OPERATOR turned off.
//
// RechargeApiKey re-enables a key whenever the post-top-up counters are under
// limit. That rule was written for the auto-deactivation case: RecordApiKeyUsage
// sets Enabled=false on quota exhaustion, and a top-up should undo exactly that.
// But Enabled=false has TWO causes — quota exhaustion and a deliberate operator
// disable (UpdateApiKey / the admin panel toggle, used for abuse, chargebacks,
// or a disputed order). Nothing in the entry distinguishes them, so a top-up on
// a key that was BANNED BY A HUMAN silently hands the credential back.
//
// This is the same defect class as checkpoint #39: an automated recovery path
// overriding an operator's deliberate quarantine because the state it keys off
// (Enabled=false) is overloaded.
//
// Scenario pinned here: a metered key WELL under its quota (so exhaustion was
// never the reason it is off) is disabled by an operator, then recharged. A
// recharge that flips Enabled back to true is the bug.
func TestRechargeDoesNotReenableOperatorDisabledKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	entry, err := AddApiKey(ApiKeyEntry{
		Name:        "abusive-buyer",
		Key:         GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: 1000,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	// Spend a little, so the key is nowhere near its limit. This removes quota
	// exhaustion as an explanation for the disable that follows.
	if err := RecordApiKeyUsage(entry.ID, 0, 10, "model-x"); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}
	if e := GetApiKeyEntry(entry.ID); e == nil || !e.Enabled {
		t.Fatalf("precondition: key should still be enabled after light usage")
	}

	// Operator disables the key deliberately (abuse / chargeback).
	patch := *GetApiKeyEntry(entry.ID)
	patch.Enabled = false
	if err := UpdateApiKey(entry.ID, patch); err != nil {
		t.Fatalf("UpdateApiKey(disable): %v", err)
	}
	if e := GetApiKeyEntry(entry.ID); e == nil || e.Enabled {
		t.Fatalf("precondition: operator disable did not take effect")
	}

	// A top-up arrives (e.g. the buyer pays again, or a bot retries an order).
	updated, err := RechargeApiKey(entry.ID, 500, 0)
	if err != nil {
		t.Fatalf("RechargeApiKey: %v", err)
	}

	if updated.Enabled {
		t.Fatalf("recharge re-enabled an operator-disabled key: a deliberate " +
			"quarantine was lifted by a top-up, so a banned buyer can restore " +
			"their own access by paying again")
	}
	if got := GetApiKeyEntry(entry.ID); got == nil || got.Enabled {
		t.Fatalf("persisted entry was re-enabled by recharge")
	}
	// The limit must still have been raised — refusing to re-enable is not a
	// reason to drop the credits the operator/buyer paid for.
	if updated.CreditLimit != 1500 {
		t.Fatalf("CreditLimit = %v, want 1500 (top-up must still apply)", updated.CreditLimit)
	}
}

// Positive control: the auto-deactivation case that the re-enable rule exists
// for must KEEP working. A key disabled purely because it hit its quota is
// re-enabled by a top-up that puts it back under limit.
//
// Without this control, "never re-enable" would look like a valid fix while
// actually breaking the recharge feature's entire purpose.
func TestRechargeStillRevivesQuotaExhaustedKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	entry, err := AddApiKey(ApiKeyEntry{
		Name:        "honest-buyer",
		Key:         GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: 100,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	// Consume the entire quota: RecordApiKeyUsage auto-disables at the limit.
	if err := RecordApiKeyUsage(entry.ID, 0, 100, "model-x"); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}
	exhausted := GetApiKeyEntry(entry.ID)
	if exhausted == nil || exhausted.Enabled {
		t.Fatalf("precondition: key should be auto-disabled at quota")
	}

	updated, err := RechargeApiKey(entry.ID, 50, 0)
	if err != nil {
		t.Fatalf("RechargeApiKey: %v", err)
	}
	if !updated.Enabled {
		t.Fatalf("recharge failed to revive a quota-exhausted key; that is the " +
			"whole point of the endpoint")
	}
}
