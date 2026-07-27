package proxy

import (
	"testing"
)

// Tenant isolation was previously tested only at the KEY DERIVATION level
// (TestResponseCacheKey_TenantIsolated: two api-key identities hash differently).
// That is necessary but not sufficient: the LRU rewrite added a second data
// structure (order *list.List) alongside the map, and eviction now deletes from
// both. A bug in that pairing could serve one tenant's stored body under
// another tenant's lookup even with correct keys — for example if an eviction
// removed a map entry while leaving a list element that a later insert reused.
//
// These tests exercise the STORE, not the hash, so the isolation property is
// pinned end to end. A cross-tenant hit here is a customer-visible data leak,
// which is the most serious defect this cache could have.
func TestResponseCacheStoreKeepsTenantsSeparate(t *testing.T) {
	c := newResponseCache()
	const now = 1000

	body := []byte(`{"same":"request"}`)
	keyA := responseCacheKey("tenant-A", "claude", body)
	keyB := responseCacheKey("tenant-B", "claude", body)

	if keyA == keyB {
		t.Fatal("identical bodies from different tenants produced the same key")
	}

	c.Set(keyA, []byte(`{"answer":"for-A"}`), 60, now)

	// Tenant B asks the identical question. It must MISS: A's answer is not B's.
	if got, ok := c.Get(keyB, now); ok {
		t.Fatalf("tenant B was served tenant A's cached response: %s", got)
	}

	// And A must still get its own answer back.
	got, ok := c.Get(keyA, now)
	if !ok {
		t.Fatal("tenant A lost its own cached response")
	}
	if string(got) != `{"answer":"for-A"}` {
		t.Fatalf("tenant A got %s, want its own body", got)
	}
}

// Isolation must survive eviction pressure. Filling the cache past capacity
// forces the LRU path to delete from both the map and the list many times; if
// that pairing is wrong, a surviving list element could be re-associated with a
// different key. After the churn, each tenant must still see only its own body.
func TestResponseCacheTenantIsolationSurvivesEviction(t *testing.T) {
	c := newResponseCache()
	const now = 1000

	body := []byte(`{"q":"shared"}`)
	keyA := responseCacheKey("tenant-A", "claude", body)
	keyB := responseCacheKey("tenant-B", "claude", body)

	c.Set(keyA, []byte(`A-body`), 3600, now)
	c.Set(keyB, []byte(`B-body`), 3600, now)

	// Churn well past capacity with unrelated keys, re-touching A and B so they
	// stay recent enough to survive.
	for i := 0; i < responseCacheMaxEntries*2; i++ {
		filler := responseCacheKey("filler", "claude", []byte{byte(i), byte(i >> 8), byte(i >> 16)})
		c.Set(filler, []byte(`filler`), 3600, now)
		if i%64 == 0 {
			c.Get(keyA, now)
			c.Get(keyB, now)
		}
	}

	// Whatever survived, it must never be the WRONG tenant's body.
	if got, ok := c.Get(keyA, now); ok && string(got) != "A-body" {
		t.Fatalf("tenant A key returned %q after eviction churn, want A-body", got)
	}
	if got, ok := c.Get(keyB, now); ok && string(got) != "B-body" {
		t.Fatalf("tenant B key returned %q after eviction churn, want B-body", got)
	}

	// The bound must still hold after all that churn.
	if n := c.Len(); n > responseCacheMaxEntries {
		t.Fatalf("cache holds %d entries, above the %d cap", n, responseCacheMaxEntries)
	}
}

// The endpoint tag is the other namespace dimension: the same tenant asking the
// same question on the Claude and OpenAI surfaces must not share a cached body,
// because the two wire formats are different shapes entirely.
func TestResponseCacheStoreKeepsEndpointsSeparate(t *testing.T) {
	c := newResponseCache()
	const now = 1000

	body := []byte(`{"q":"same"}`)
	claudeKey := responseCacheKey("tenant-A", "claude", body)
	openaiKey := responseCacheKey("tenant-A", "openai", body)

	if claudeKey == openaiKey {
		t.Fatal("claude and openai namespaces collided for the same tenant+body")
	}

	c.Set(claudeKey, []byte(`{"content":[]}`), 60, now)
	if got, ok := c.Get(openaiKey, now); ok {
		t.Fatalf("openai lookup served a claude-shaped cached body: %s", got)
	}
}
