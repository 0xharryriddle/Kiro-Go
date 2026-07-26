package proxy

import (
	"fmt"
	"testing"
)

// The response cache (F5) stores whole HTTP response bodies keyed by a SHA-256
// of (apiKeyID, endpoint, normalized request body). Nothing bounds it.
//
// Expiry is checked ONLY when the same key is looked up again (Get deletes a
// stale entry lazily). A key that is never queried a second time is therefore
// never removed — and the whole point of an exact-match cache key is that a
// distinct request produces a distinct key. So every non-repeating request
// permanently adds a full response body to the map.
//
// Concretely: a client sending unique prompts (the normal case for a coding
// agent) grows this map without limit for as long as the process lives. At a
// few KiB per stored completion, a busy day is gigabytes of retained heap that
// no request will ever read. The container has no memory limit set in
// docker-compose.yml, so the OOM killer takes the whole proxy down and every
// pooled account goes offline with it.
//
// The sibling prompt-cache tracker already solved exactly this problem the
// right way (cache_tracker.go: container/list LRU + maxEntries + eviction
// counters). This cache was written without any of it.
func TestResponseCacheIsCapacityBounded(t *testing.T) {
	c := newResponseCache()
	const ttl = 300
	const now = 1000

	// Every key distinct, none ever re-read: the shape real traffic produces.
	const inserted = responseCacheMaxEntries * 3
	for i := 0; i < inserted; i++ {
		c.Set(fmt.Sprintf("unique-key-%d", i), []byte("a stored response body"), ttl, now)
	}

	if got := c.Len(); got > responseCacheMaxEntries {
		t.Fatalf("cache grew to %d entries with no bound (cap is %d): a stream of unique requests exhausts memory",
			got, responseCacheMaxEntries)
	}
}

// Bounding must not break the cache: a freshly stored entry has to remain
// retrievable. This is the guard against "fixing" the bound by discarding
// everything.
func TestResponseCacheStillServesRecentEntries(t *testing.T) {
	c := newResponseCache()
	const ttl = 300
	const now = 1000

	c.Set("hot", []byte("hot body"), ttl, now)
	got, ok := c.Get("hot", now+1)
	if !ok {
		t.Fatal("a just-stored entry was not retrievable")
	}
	if string(got) != "hot body" {
		t.Fatalf("wrong body: %q", got)
	}
}

// Eviction must be least-recently-USED, not arbitrary: an entry that keeps
// being read is the one worth keeping. Without recency tracking a hot entry can
// be thrown away while cold ones survive, which quietly destroys the hit rate
// the cache exists to produce.
func TestResponseCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newResponseCache()
	const ttl = 300
	const now = 1000

	c.Set("keep-me", []byte("hot"), ttl, now)
	// Keep it hot while the cache is driven past capacity.
	for i := 0; i < responseCacheMaxEntries*2; i++ {
		if i%8 == 0 {
			if _, ok := c.Get("keep-me", now+1); !ok {
				t.Fatalf("hot entry evicted after %d inserts despite continuous reads", i)
			}
		}
		c.Set(fmt.Sprintf("cold-%d", i), []byte("cold"), ttl, now)
	}

	if _, ok := c.Get("keep-me", now+1); !ok {
		t.Fatal("the most-recently-used entry was evicted while colder entries survived")
	}
}

// Expired entries must be reclaimable without needing a second lookup of the
// same key, or a burst of one-shot requests leaves dead bodies pinned until
// eviction pressure happens to reach them.
func TestResponseCacheReclaimsExpiredWithoutRelookup(t *testing.T) {
	c := newResponseCache()
	const ttl = 10
	const now = 1000

	for i := 0; i < 64; i++ {
		c.Set(fmt.Sprintf("stale-%d", i), []byte("body"), ttl, now)
	}
	if c.Len() == 0 {
		t.Fatal("setup failed: nothing stored")
	}

	// Well past every TTL. Storing one new entry must be enough to reclaim.
	c.Set("fresh", []byte("body"), ttl, now+ttl+1)

	if got := c.Len(); got > 1 {
		t.Fatalf("expired entries were retained: %d entries remain after all but one expired", got)
	}
}
