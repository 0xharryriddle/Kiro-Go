package pool

import (
	"strconv"
	"testing"
	"time"
)

// evictOldestAffinityLocked runs under the pool WRITE lock, on the routing hot
// path, and it is O(n) per eviction. Once the affinity map is saturated with
// fresh bindings every new API key triggers one full scan of 1024 entries, so
// the cost is worth measuring rather than assuming: if a scan were expensive it
// would serialize every request in the proxy behind it.
//
// This is a benchmark, not a gate — it exists so the cost is a number someone
// can look at instead of an argument.
func BenchmarkEvictOldestAffinityAtCap(b *testing.B) {
	p := &AccountPool{apiKeyAffinity: make(map[string]apiKeyBinding, maxAffinityEntries)}
	base := time.Now()
	for i := 0; i < maxAffinityEntries; i++ {
		p.apiKeyAffinity["key-"+strconv.Itoa(i)] = apiKeyBinding{
			accountID: "a",
			lastUsed:  base.Add(time.Duration(i) * time.Millisecond),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Evict one, then re-add so the map stays exactly at the cap and every
		// iteration pays the same full-scan cost.
		p.evictOldestAffinityLocked(maxAffinityEntries - 1)
		p.apiKeyAffinity["fresh-"+strconv.Itoa(i)] = apiKeyBinding{
			accountID: "a",
			lastUsed:  base.Add(time.Duration(maxAffinityEntries+i) * time.Millisecond),
		}
	}
}

// The eviction must actually converge: a bug that failed to shrink the map would
// spin forever under the write lock and deadlock the proxy. This pins
// termination and the exact resulting size.
func TestEvictOldestAffinityConverges(t *testing.T) {
	p := &AccountPool{apiKeyAffinity: make(map[string]apiKeyBinding)}
	base := time.Now()
	for i := 0; i < 50; i++ {
		p.apiKeyAffinity["key-"+strconv.Itoa(i)] = apiKeyBinding{
			accountID: "a",
			lastUsed:  base.Add(time.Duration(i) * time.Second),
		}
	}

	p.evictOldestAffinityLocked(10)
	if got := len(p.apiKeyAffinity); got != 10 {
		t.Fatalf("evict did not converge to target: len = %d, want 10", got)
	}

	// The survivors must be the NEWEST bindings — evicting the freshest would
	// defeat the purpose (it would drop the sessions currently in use).
	for key := range p.apiKeyAffinity {
		n, err := strconv.Atoi(key[len("key-"):])
		if err != nil {
			t.Fatalf("unexpected key %q", key)
		}
		if n < 40 {
			t.Fatalf("evicted a newer binding while keeping older key-%d", n)
		}
	}
}

// Guard the degenerate inputs: a target of zero empties the map, and a negative
// target must not loop past empty.
func TestEvictOldestAffinityHandlesDegenerateTargets(t *testing.T) {
	p := &AccountPool{apiKeyAffinity: make(map[string]apiKeyBinding)}
	p.apiKeyAffinity["a"] = apiKeyBinding{accountID: "x", lastUsed: time.Now()}
	p.apiKeyAffinity["b"] = apiKeyBinding{accountID: "x", lastUsed: time.Now()}

	p.evictOldestAffinityLocked(-5)
	if got := len(p.apiKeyAffinity); got != 0 {
		t.Fatalf("negative target should empty the map, len = %d", got)
	}

	// And on an already-empty map it must return rather than spin.
	p.evictOldestAffinityLocked(0)
}
