package proxy

// Response cache (F5): an in-process exact-match cache for NON-STREAMING,
// tool-free, non-thinking chat/messages responses. Every hit avoids an upstream
// call, saving one credit — the scarce resource this proxy manages.
//
// SECURITY / CORRECTNESS (why this is safe to enable):
//   - Opt-in, default OFF. Prior behaviour is unchanged until enabled.
//   - Only exact-match: the key is a SHA-256 over the endpoint + fully normalized
//     request body, so ANY difference in model, messages, system, or sampling
//     params misses. No fuzzy/semantic matching.
//   - Never caches streaming, tool, or thinking requests — those are gated out by
//     the caller before we are consulted (isCacheableRequest).
//   - The cache is keyed by the authenticated API-key identity in ADDITION to the
//     request content, so one API key's cached response can never be served to a
//     different key (tenant isolation). Requests with no API key (auth disabled or
//     the legacy single-key path) share the empty-identity namespace, which is the
//     same trust boundary they already share.
//   - TTL-bounded and in-memory only (single-instance); nothing is persisted.

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
)

// cachedResponse holds one stored response body with its expiry.
type cachedResponse struct {
	body      []byte
	expiresAt int64         // Unix seconds
	lruElem   *list.Element // back-ref into responseCache.order; Value = cache key
}

// responseCacheMaxEntries bounds how many response bodies the cache retains.
// Whole HTTP bodies are stored here, so this is a memory ceiling rather than a
// count that can be set generously: 2048 completions at a few KiB each is on
// the order of low tens of MiB, which is affordable, while an unbounded map is
// not.
const responseCacheMaxEntries = 2048

// responseCache is the in-process TTL store. Safe for concurrent use.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]cachedResponse
	// order is the LRU list: front = most recently used. Element.Value is the
	// cache key. Mirrors the design already proven in cache_tracker.go rather
	// than inventing a second eviction scheme.
	order *list.List
}

func newResponseCache() *responseCache {
	return &responseCache{
		entries: make(map[string]cachedResponse),
		order:   list.New(),
	}
}

// Len reports the number of retained entries. Used by tests and by the stats
// endpoint to make cache growth observable rather than a guess.
func (c *responseCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// responseCacheKey derives the cache key from the authenticated API-key identity,
// the endpoint tag, and the raw, already-normalized request body. Pure and
// unit-testable. The endpoint tag keeps the openai/claude/responses namespaces
// separate even if two bodies coincide; the apiKeyID prefix keeps one tenant's
// cached responses from ever being served to another key (an empty apiKeyID is
// its own namespace, shared by auth-disabled and legacy single-key requests).
func responseCacheKey(apiKeyID, endpoint string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	h.Write([]byte{0})
	h.Write([]byte(endpoint))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns a fresh cached body for the key, or (nil, false) on miss/expiry.
// A hit marks the entry most-recently-used. `now` is injectable for tests.
func (c *responseCache) Get(key string, now int64) ([]byte, bool) {
	if key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if now >= e.expiresAt {
		c.removeLocked(key, e)
		return nil, false
	}
	c.order.MoveToFront(e.lruElem)
	return e.body, true
}

// Set stores a response body under the key with the given TTL. A copy of the body
// is retained so later mutation of the caller's buffer cannot corrupt the cache.
// `now` is injectable for tests. No-op for empty key/body or non-positive TTL.
//
// Two reclamation steps run on every insert, because insertion is the ONLY point
// at which this cache grows:
//
//   - Expired entries are swept. Reclaiming them lazily in Get was not enough:
//     Get only ever inspects the one key being looked up, so an entry whose key
//     is never requested again is never examined again and its body is retained
//     for the process's lifetime. A stream of distinct requests — exactly the
//     traffic shape a proxy sees — therefore accumulated dead bodies forever.
//   - Overflow beyond responseCacheMaxEntries is evicted least-recently-used.
//     Whole HTTP response bodies live in this map, so an unbounded map is an
//     unbounded memory leak that ends in the OOM killer taking down the proxy.
//     The TTL alone does not bound it (see above).
//
// The sweep is O(n) but runs only on insert and only over the map, which the
// eviction below keeps at responseCacheMaxEntries — so the work is bounded by a
// constant, not by traffic volume.
func (c *responseCache) Set(key string, body []byte, ttlSeconds int, now int64) {
	if key == "" || len(body) == 0 || ttlSeconds <= 0 {
		return
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sweepExpiredLocked(now)

	if e, ok := c.entries[key]; ok {
		// Refresh in place: keep the existing LRU element, move it to front.
		e.body = cp
		e.expiresAt = now + int64(ttlSeconds)
		c.entries[key] = e
		c.order.MoveToFront(e.lruElem)
		return
	}

	elem := c.order.PushFront(key)
	c.entries[key] = cachedResponse{
		body:      cp,
		expiresAt: now + int64(ttlSeconds),
		lruElem:   elem,
	}
	c.evictOverflowLocked()
}

// sweepExpiredLocked drops every entry whose TTL has elapsed. Caller holds c.mu.
func (c *responseCache) sweepExpiredLocked(now int64) {
	for key, e := range c.entries {
		if now >= e.expiresAt {
			c.removeLocked(key, e)
		}
	}
}

// evictOverflowLocked evicts least-recently-used entries until the map is within
// responseCacheMaxEntries. O(1) per eviction. Caller holds c.mu.
func (c *responseCache) evictOverflowLocked() {
	for len(c.entries) > responseCacheMaxEntries {
		back := c.order.Back()
		if back == nil {
			return
		}
		key := back.Value.(string)
		if e, ok := c.entries[key]; ok {
			c.removeLocked(key, e)
			continue
		}
		// Defensive: list element with no map entry — drop the orphan so the
		// loop cannot spin.
		c.order.Remove(back)
	}
}

// removeLocked deletes an entry and its LRU element together, so the two
// structures cannot drift out of sync. Caller holds c.mu.
func (c *responseCache) removeLocked(key string, e cachedResponse) {
	if e.lruElem != nil {
		c.order.Remove(e.lruElem)
	}
	delete(c.entries, key)
}

// usageFromCachedOpenAIBody parses the prompt/completion token counts out of a
// cached OpenAI chat-completions response body so a cache hit can be attributed
// to the tenant's usage. Returns (0, 0) on any parse failure — a cache hit must
// never fail the request just because its usage could not be re-derived.
func usageFromCachedOpenAIBody(body []byte) (inputTokens, outputTokens int) {
	var parsed struct {
		Usage OpenAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, 0
	}
	return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens
}

// usageFromCachedClaudeBody parses the input/output token counts out of a cached
// Claude messages response body. Returns (0, 0) on any parse failure.
func usageFromCachedClaudeBody(body []byte) (inputTokens, outputTokens int) {
	var parsed struct {
		Usage ClaudeUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, 0
	}
	return parsed.Usage.InputTokens, parsed.Usage.OutputTokens
}

// isCacheableClaudeRequest reports whether a Claude request may be cached: not
// streaming, no tools, and thinking disabled. These are the correctness gates
// from the design doc — cache only deterministic, single-shot completions.
func isCacheableClaudeRequest(req *ClaudeRequest, thinking bool) bool {
	if req == nil || req.Stream || thinking {
		return false
	}
	if len(req.Tools) > 0 {
		return false
	}
	return true
}

// isCacheableOpenAIRequest reports whether an OpenAI request may be cached: not
// streaming, no tools, thinking disabled.
func isCacheableOpenAIRequest(req *OpenAIRequest, thinking bool) bool {
	if req == nil || req.Stream || thinking {
		return false
	}
	if len(req.Tools) > 0 {
		return false
	}
	return true
}

// captureWriter is an http.ResponseWriter that tees the response body into a
// buffer while forwarding to the real writer, so a successful (200) response can
// be stored in the cache after it is sent. It records the status code so we only
// cache clean successes.
type captureWriter struct {
	http.ResponseWriter
	status int
	buf    []byte
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
	return &captureWriter{ResponseWriter: w, status: http.StatusOK}
}

func (cw *captureWriter) WriteHeader(status int) {
	cw.status = status
	cw.ResponseWriter.WriteHeader(status)
}

func (cw *captureWriter) Write(b []byte) (int, error) {
	// Only retain the body for cacheable (200) responses to bound memory.
	if cw.status == http.StatusOK {
		cw.buf = append(cw.buf, b...)
	}
	return cw.ResponseWriter.Write(b)
}
