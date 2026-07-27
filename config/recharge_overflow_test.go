package config

import (
	"math"
	"path/filepath"
	"testing"
)

// RechargeApiKey validates only that amounts are non-negative (the HTTP layer
// checks `req.Credits < 0 || req.Tokens < 0`). Nothing bounds them from above,
// and the arithmetic is a bare `+=` on an int64 and a float64.
//
// Two distinct consequences, pinned separately below:
//
//  1. int64 TokenLimit WRAPS NEGATIVE. A negative limit is not "no limit" —
//     ApiKeyOverLimit compares TokensUsed against it, so the key is instantly
//     and permanently over limit: a paid top-up bricks the key it was meant to
//     extend.
//
//  2. float64 CreditLimit reaches +Inf, and +Inf CANNOT BE MARSHALLED to JSON.
//     config.saveLocked marshals the whole config, so once one key holds +Inf
//     EVERY subsequent config write fails — new accounts, key mints, usage
//     counters, ban stamps, all of it. One bad recharge poisons persistence
//     process-wide.
func TestRechargeTokenLimitDoesNotWrapNegative(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	entry, err := AddApiKey(ApiKeyEntry{
		Name:       "token-metered",
		Key:        GenerateApiKeyValue(),
		Enabled:    true,
		TokenLimit: 1000,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	// A top-up large enough to wrap. Reachable over the wire: the field is a
	// plain JSON number decoded into int64.
	updated, err := RechargeApiKey(entry.ID, 0, math.MaxInt64)
	if err != nil {
		// Refusing the overflowing top-up outright is a perfectly good fix.
		return
	}

	if updated.TokenLimit < 0 {
		t.Fatalf("TokenLimit wrapped negative (%d): the key is now permanently "+
			"over limit, so a paid top-up bricked the key it was meant to extend",
			updated.TokenLimit)
	}
}

func TestRechargeCreditLimitCannotPoisonConfigPersistence(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	entry, err := AddApiKey(ApiKeyEntry{
		Name:        "credit-metered",
		Key:         GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: 100,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	// Two near-max top-ups: the first is finite, the second overflows to +Inf.
	// math.MaxFloat64 is an ordinary JSON number, so this is wire-reachable.
	if _, err := RechargeApiKey(entry.ID, math.MaxFloat64, 0); err != nil {
		return // refused before any mutation — acceptable
	}
	// This second call is EXPECTED to fail: saveLocked cannot marshal +Inf.
	// Returning an error is not the end of the story, because the in-memory
	// cfg was mutated BEFORE the save was attempted and RechargeApiKey does
	// not roll it back. That is the defect this test is really about.
	_, rechargeErr := RechargeApiKey(entry.ID, math.MaxFloat64, 0)

	// Whatever the call returned, the STORE must not have been left holding a
	// non-finite limit.
	stored := GetApiKeyEntry(entry.ID)
	if stored == nil {
		t.Fatalf("key vanished after overflow recharge")
	}
	if math.IsInf(stored.CreditLimit, 0) || math.IsNaN(stored.CreditLimit) {
		// Prove the consequence instead of asserting it: with +Inf resident in
		// cfg, EVERY later config write fails, whatever it is about. Minting an
		// unrelated key is the cheapest demonstration.
		_, unrelatedErr := AddApiKey(ApiKeyEntry{
			Name:    "unrelated-later-key",
			Key:     GenerateApiKeyValue(),
			Enabled: true,
		})
		if unrelatedErr != nil {
			t.Fatalf("in-memory config was poisoned: CreditLimit=%v survived a "+
				"failed recharge (err=%v), and now an unrelated later write also "+
				"fails (%v) — no account, key, usage counter or ban stamp can be "+
				"persisted again for the life of the process",
				stored.CreditLimit, rechargeErr, unrelatedErr)
		}
		t.Fatalf("CreditLimit was left non-finite (%v) after a failed recharge; "+
			"limits must stay finite", stored.CreditLimit)
	}
}

// Positive control: an ordinary top-up must still work exactly as before. Any
// bound added above must not break the normal purchase path.
func TestRechargeOrdinaryTopUpStillWorks(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	entry, err := AddApiKey(ApiKeyEntry{
		Name:        "normal-buyer",
		Key:         GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: 100,
		TokenLimit:  1000,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	updated, err := RechargeApiKey(entry.ID, 50, 500)
	if err != nil {
		t.Fatalf("ordinary recharge failed: %v", err)
	}
	if updated.CreditLimit != 150 {
		t.Fatalf("CreditLimit = %v, want 150", updated.CreditLimit)
	}
	if updated.TokenLimit != 1500 {
		t.Fatalf("TokenLimit = %d, want 1500", updated.TokenLimit)
	}
}
