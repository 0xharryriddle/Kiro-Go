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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
)

// cachedResponse holds one stored response body with its expiry.
type cachedResponse struct {
	body      []byte
	expiresAt int64 // Unix seconds
}

// responseCache is the in-process TTL store. Safe for concurrent use.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]cachedResponse
}

func newResponseCache() *responseCache {
	return &responseCache{entries: make(map[string]cachedResponse)}
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
// Expired entries are dropped lazily. `now` is injectable for tests.
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
		delete(c.entries, key)
		return nil, false
	}
	return e.body, true
}

// Set stores a response body under the key with the given TTL. A copy of the body
// is retained so later mutation of the caller's buffer cannot corrupt the cache.
// `now` is injectable for tests. No-op for empty key/body or non-positive TTL.
func (c *responseCache) Set(key string, body []byte, ttlSeconds int, now int64) {
	if key == "" || len(body) == 0 || ttlSeconds <= 0 {
		return
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedResponse{body: cp, expiresAt: now + int64(ttlSeconds)}
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
