package config

import (
	"path/filepath"
	"sync"
	"testing"
)

// pool.UpdateStats computes an ABSOLUTE stats snapshot under the pool lock and
// then persists it from a detached goroutine (`go config.UpdateAccountStats(...)`,
// pool/account.go:523). Those goroutines are unordered, so a snapshot computed
// EARLIER can land AFTER a newer one and overwrite it, leaving the persisted
// request/token/credit counters permanently lower than the live in-memory values.
//
// Since the only caller derives these counters from monotonically increasing
// in-memory state, a snapshot whose requestCount is below what is already stored
// is stale by construction and must not clobber the newer value.
func TestUpdateAccountStatsIgnoresStaleSnapshot(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	// Newest snapshot lands first.
	if err := UpdateAccountStats("a", 50, 0, 500, 5.0, 1000); err != nil {
		t.Fatalf("update stats: %v", err)
	}
	// A stale goroutine from an earlier call lands afterwards.
	if err := UpdateAccountStats("a", 24, 0, 240, 2.4, 900); err != nil {
		t.Fatalf("update stats: %v", err)
	}

	got, _ := GetAccountByID("a")
	if got.RequestCount != 50 {
		t.Fatalf("stale snapshot overwrote newer stats: RequestCount=%d, want 50", got.RequestCount)
	}
	if got.TotalTokens != 500 {
		t.Fatalf("stale snapshot overwrote newer tokens: TotalTokens=%d, want 500", got.TotalTokens)
	}
	if got.TotalCredits != 5.0 {
		t.Fatalf("stale snapshot overwrote newer credits: TotalCredits=%v, want 5", got.TotalCredits)
	}
}

// A genuinely newer snapshot must still be applied in full.
func TestUpdateAccountStatsAppliesNewerSnapshot(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	if err := UpdateAccountStats("a", 10, 1, 100, 1.0, 900); err != nil {
		t.Fatalf("update stats: %v", err)
	}
	if err := UpdateAccountStats("a", 11, 2, 110, 1.1, 1000); err != nil {
		t.Fatalf("update stats: %v", err)
	}

	got, _ := GetAccountByID("a")
	if got.RequestCount != 11 || got.ErrorCount != 2 || got.TotalTokens != 110 || got.LastUsed != 1000 {
		t.Fatalf("newer snapshot not applied: %+v", got)
	}
}

// Concurrent out-of-order persistence must converge on the highest snapshot,
// which is the shape pool.UpdateStats actually produces under load.
func TestUpdateAccountStatsConvergesUnderConcurrency(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	var wg sync.WaitGroup
	const n = 50
	wg.Add(n)
	for i := 1; i <= n; i++ {
		go func(count int) {
			defer wg.Done()
			_ = UpdateAccountStats("a", count, 0, count*10, float64(count), int64(count))
		}(i)
	}
	wg.Wait()

	got, _ := GetAccountByID("a")
	if got.RequestCount != n {
		t.Fatalf("did not converge on the highest snapshot: RequestCount=%d, want %d", got.RequestCount, n)
	}
}
