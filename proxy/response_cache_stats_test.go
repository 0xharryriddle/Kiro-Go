package proxy

import (
	"testing"
)

// The response cache was the one cache in this proxy with no observability at
// all: no counters, no /v1/stats entry, no admin field. That is not a cosmetic
// gap. An operator enabling F5 has no way to answer the only two questions that
// decide whether it is worth having on:
//
//   - Is it saving credits? (hits)
//   - Is it thrashing? (evictions, i.e. the 2048-entry bound is too small for
//     this traffic and entries are dropped before they are ever reused)
//
// Without those numbers the feature can only be evaluated by guesswork, and a
// cache that silently never hits is indistinguishable from one that is off. The
// prompt-cache tracker already exposes exactly this counter set
// (PromptCacheStats), so this mirrors it rather than inventing a second shape.

// T1 — a miss is counted as a miss.
func TestResponseCacheCountsMisses(t *testing.T) {
	c := newResponseCache()
	if _, ok := c.Get("absent", 100); ok {
		t.Fatal("empty cache returned a hit")
	}
	s := c.Stats()
	if s.Misses != 1 {
		t.Fatalf("misses = %d, want 1", s.Misses)
	}
	if s.Hits != 0 {
		t.Fatalf("hits = %d, want 0", s.Hits)
	}
}

// T2 — a hit is counted as a hit, and does not also count a miss.
func TestResponseCacheCountsHits(t *testing.T) {
	c := newResponseCache()
	c.Set("k", []byte("body"), 60, 100)
	if _, ok := c.Get("k", 101); !ok {
		t.Fatal("fresh entry did not hit")
	}
	s := c.Stats()
	if s.Hits != 1 {
		t.Fatalf("hits = %d, want 1", s.Hits)
	}
	if s.Misses != 0 {
		t.Fatalf("misses = %d, want 0", s.Misses)
	}
}

// T3 — an expired lookup counts as a miss AND an expiration, not as a hit.
// These are separate facts: "the client asked for something we no longer had"
// versus "an entry aged out". Conflating them hides whether the TTL is too
// short for the traffic.
func TestResponseCacheCountsExpirationSeparatelyFromMiss(t *testing.T) {
	c := newResponseCache()
	c.Set("k", []byte("body"), 10, 100)

	if _, ok := c.Get("k", 200); ok { // well past expiry
		t.Fatal("expired entry returned a hit")
	}
	s := c.Stats()
	if s.Misses != 1 {
		t.Fatalf("misses = %d, want 1", s.Misses)
	}
	if s.Expirations != 1 {
		t.Fatalf("expirations = %d, want 1", s.Expirations)
	}
	if s.Hits != 0 {
		t.Fatalf("hits = %d, want 0", s.Hits)
	}
}

// T4 — LRU evictions are counted, so an operator can see the bound biting.
func TestResponseCacheCountsEvictions(t *testing.T) {
	c := newResponseCache()
	// Overfill by a known margin.
	const overflow = 50
	for i := 0; i < responseCacheMaxEntries+overflow; i++ {
		c.Set(keyForIndex(i), []byte("b"), 3600, 1000)
	}
	s := c.Stats()
	if s.Evictions < overflow {
		t.Fatalf("evictions = %d, want >= %d", s.Evictions, overflow)
	}
	if s.Entries > responseCacheMaxEntries {
		t.Fatalf("entries = %d exceeds cap %d", s.Entries, responseCacheMaxEntries)
	}
}

// T5 — the sweep-on-insert path also counts expirations. Reclaiming an entry
// nobody looked up is still an expiration and must be visible; otherwise the
// counter only reflects re-requested keys and understates TTL churn.
func TestResponseCacheCountsSweptExpirations(t *testing.T) {
	c := newResponseCache()
	for i := 0; i < 20; i++ {
		c.Set(keyForIndex(i), []byte("b"), 10, 100)
	}
	// A later insert sweeps all 20 expired entries without any Get.
	c.Set("fresh", []byte("b"), 60, 500)

	s := c.Stats()
	if s.Expirations < 20 {
		t.Fatalf("expirations = %d, want >= 20 from the insert sweep", s.Expirations)
	}
}

// T6 — Entries and Capacity report the live shape.
func TestResponseCacheStatsReportSizeAndCapacity(t *testing.T) {
	c := newResponseCache()
	c.Set("a", []byte("b"), 60, 100)
	c.Set("b", []byte("b"), 60, 100)
	s := c.Stats()
	if s.Entries != 2 {
		t.Fatalf("entries = %d, want 2", s.Entries)
	}
	if s.Capacity != responseCacheMaxEntries {
		t.Fatalf("capacity = %d, want %d", s.Capacity, responseCacheMaxEntries)
	}
}

// T7 — counters are cumulative, not reset by later operations.
func TestResponseCacheCountersAreCumulative(t *testing.T) {
	c := newResponseCache()
	c.Set("k", []byte("b"), 60, 100)
	for i := 0; i < 3; i++ {
		c.Get("k", 101)
	}
	for i := 0; i < 4; i++ {
		c.Get("nope", 101)
	}
	s := c.Stats()
	if s.Hits != 3 {
		t.Fatalf("hits = %d, want 3", s.Hits)
	}
	if s.Misses != 4 {
		t.Fatalf("misses = %d, want 4", s.Misses)
	}
}

// T8 — a nil cache must not panic. The handler holds a pointer and a
// zero-valued Handler (used widely in tests) leaves it nil; Stats() is called
// from the stats endpoint, which must never be the thing that crashes the proxy.
func TestResponseCacheStatsNilSafe(t *testing.T) {
	var c *responseCache
	s := c.Stats() // must not panic
	if s.Entries != 0 || s.Hits != 0 || s.Misses != 0 {
		t.Fatalf("nil cache reported non-zero stats: %+v", s)
	}
}

// keyForIndex builds a distinct, deterministic key. Kept local so the bound
// tests and these stats tests do not depend on each other's helpers.
func keyForIndex(i int) string {
	const digits = "0123456789abcdef"
	buf := make([]byte, 0, 12)
	buf = append(buf, 'k')
	n := i
	for n > 0 || len(buf) == 1 {
		buf = append(buf, digits[n&0xf])
		n >>= 4
		if n == 0 {
			break
		}
	}
	return string(buf)
}
