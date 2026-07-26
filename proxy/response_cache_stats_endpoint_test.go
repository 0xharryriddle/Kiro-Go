package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	accountpool "kiro-go/pool"
)

// The counters are only worth having if an operator can read them. Before this,
// /v1/stats reported the prompt-cache tracker under "cache" and said nothing at
// all about the response cache — so the question "is the response cache saving
// me credits?" had no answer short of reading source.
//
// This drives the REAL handler through the REAL route so a future refactor that
// drops the field from the payload fails here rather than silently going dark.
func TestStatsEndpointExposesResponseCache(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{pool: accountpool.GetPool(), responseCache: newResponseCache()}

	// Give the cache observable state: one miss, one store, one hit.
	key := responseCacheKey("k", "claude", []byte(`{"model":"m"}`))
	h.responseCache.Get(key, 1000)                         // miss
	h.responseCache.Set(key, []byte(`{"ok":1}`), 60, 1000) // store
	h.responseCache.Get(key, 1001)                         // hit

	rec := httptest.NewRecorder()
	h.handleStats(rec, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("stats body is not JSON: %v", err)
	}

	raw, ok := payload["responseCache"]
	if !ok {
		t.Fatalf("stats payload has no responseCache block; keys present: %v", keysOf(payload))
	}
	block, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("responseCache is not an object: %#v", raw)
	}

	// Every counter an operator needs to answer "is it working?" must be present.
	for _, field := range []string{"enabled", "entries", "capacity", "ttlSeconds", "hits", "misses", "evictions", "expirations"} {
		if _, ok := block[field]; !ok {
			t.Errorf("responseCache missing field %q", field)
		}
	}

	if got := block["hits"]; got != float64(1) {
		t.Errorf("hits = %v, want 1", got)
	}
	if got := block["misses"]; got != float64(1) {
		t.Errorf("misses = %v, want 1", got)
	}
	if got := block["entries"]; got != float64(1) {
		t.Errorf("entries = %v, want 1", got)
	}
}

// The prompt-cache block must NOT be displaced by the new one: they measure
// different mechanisms and an operator needs both.
func TestStatsEndpointStillExposesPromptCache(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{
		pool:          accountpool.GetPool(),
		responseCache: newResponseCache(),
		promptCache:   newPromptCacheTracker(defaultPromptCacheTTL),
	}

	rec := httptest.NewRecorder()
	h.handleStats(rec, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))

	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("stats body is not JSON: %v", err)
	}
	if _, ok := payload["cache"]; !ok {
		t.Fatalf("prompt-cache block disappeared from stats; keys: %v", keysOf(payload))
	}
	if _, ok := payload["responseCache"]; !ok {
		t.Fatalf("responseCache block missing; keys: %v", keysOf(payload))
	}
}

// A zero-valued Handler (nil caches) must not panic the stats endpoint: the
// health/observability path must never be the thing that takes the proxy down.
func TestStatsEndpointNilCachesDoNotPanic(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{pool: accountpool.GetPool()}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stats endpoint panicked with nil caches: %v", r)
		}
	}()

	rec := httptest.NewRecorder()
	h.handleStats(rec, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
