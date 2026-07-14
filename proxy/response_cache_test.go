package proxy

import "testing"

func TestResponseCacheKey_StableAndNamespaced(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	k1 := responseCacheKey("key-a", "openai", body)
	k2 := responseCacheKey("key-a", "openai", body)
	if k1 != k2 {
		t.Fatalf("expected stable key, got %q vs %q", k1, k2)
	}
	// Different endpoint namespace -> different key even for identical bodies.
	if responseCacheKey("key-a", "claude", body) == k1 {
		t.Fatalf("expected endpoint namespace to change the key")
	}
	// Different body -> different key.
	if responseCacheKey("key-a", "openai", []byte(`{"model":"m2"}`)) == k1 {
		t.Fatalf("expected body difference to change the key")
	}
}

// TestResponseCacheKey_TenantIsolated verifies a different API-key identity
// yields a different cache key for the same endpoint+body, so one tenant's
// cached response can never be served to another key. The empty identity is its
// own namespace (auth-disabled / legacy single-key requests).
func TestResponseCacheKey_TenantIsolated(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	kA := responseCacheKey("key-a", "openai", body)
	kB := responseCacheKey("key-b", "openai", body)
	kEmpty := responseCacheKey("", "openai", body)
	if kA == kB {
		t.Fatalf("expected different API keys to produce different cache keys")
	}
	if kA == kEmpty || kB == kEmpty {
		t.Fatalf("expected empty identity to be its own namespace")
	}
}

func TestResponseCacheKey_EmptyForNoInput(t *testing.T) {
	// A key is always derived (sha256 of identity+endpoint+body); empty body
	// still yields a deterministic non-empty key.
	if responseCacheKey("", "openai", nil) == "" {
		t.Fatalf("expected non-empty key even for nil body")
	}
}

func TestUsageFromCachedOpenAIBody(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`)
	in, out := usageFromCachedOpenAIBody(body)
	if in != 12 || out != 34 {
		t.Fatalf("expected (12,34), got (%d,%d)", in, out)
	}
	// Malformed body must not panic and yields zero usage.
	if in, out := usageFromCachedOpenAIBody([]byte("not json")); in != 0 || out != 0 {
		t.Fatalf("expected (0,0) on parse failure, got (%d,%d)", in, out)
	}
}

func TestUsageFromCachedClaudeBody(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":7,"output_tokens":9}}`)
	in, out := usageFromCachedClaudeBody(body)
	if in != 7 || out != 9 {
		t.Fatalf("expected (7,9), got (%d,%d)", in, out)
	}
	if in, out := usageFromCachedClaudeBody([]byte("{")); in != 0 || out != 0 {
		t.Fatalf("expected (0,0) on parse failure, got (%d,%d)", in, out)
	}
}

func TestResponseCacheGetSetAndExpiry(t *testing.T) {
	c := newResponseCache()
	key := "k1"
	c.Set(key, []byte("hello"), 100, 1000)

	if got, ok := c.Get(key, 1050); !ok || string(got) != "hello" {
		t.Fatalf("expected fresh hit, got %q ok=%v", got, ok)
	}
	// At/after expiry -> miss.
	if _, ok := c.Get(key, 1100); ok {
		t.Fatalf("expected expiry at now>=expiresAt")
	}
	// Expired entry is dropped.
	if _, ok := c.Get(key, 1050); ok {
		t.Fatalf("expected entry dropped after expiry read")
	}
}

func TestResponseCacheSetNoopOnBadInput(t *testing.T) {
	c := newResponseCache()
	c.Set("", []byte("x"), 100, 1000) // empty key
	c.Set("k", nil, 100, 1000)        // empty body
	c.Set("k2", []byte("x"), 0, 1000) // non-positive TTL
	if _, ok := c.Get("k2", 1000); ok {
		t.Fatalf("expected no store on non-positive TTL")
	}
}

func TestResponseCacheSetCopiesBody(t *testing.T) {
	c := newResponseCache()
	body := []byte("abc")
	c.Set("k", body, 100, 1000)
	body[0] = 'X' // mutate caller's buffer after Set
	got, ok := c.Get("k", 1000)
	if !ok || string(got) != "abc" {
		t.Fatalf("expected cache to retain an unmutated copy, got %q", got)
	}
}

func TestIsCacheableClaudeRequest(t *testing.T) {
	if !isCacheableClaudeRequest(&ClaudeRequest{}, false) {
		t.Fatalf("plain non-stream request should be cacheable")
	}
	if isCacheableClaudeRequest(&ClaudeRequest{Stream: true}, false) {
		t.Fatalf("streaming must not be cacheable")
	}
	if isCacheableClaudeRequest(&ClaudeRequest{}, true) {
		t.Fatalf("thinking must not be cacheable")
	}
	if isCacheableClaudeRequest(&ClaudeRequest{Tools: []ClaudeTool{{Name: "t"}}}, false) {
		t.Fatalf("tool requests must not be cacheable")
	}
	if isCacheableClaudeRequest(nil, false) {
		t.Fatalf("nil request must not be cacheable")
	}
}

func TestIsCacheableOpenAIRequest(t *testing.T) {
	if !isCacheableOpenAIRequest(&OpenAIRequest{}, false) {
		t.Fatalf("plain non-stream request should be cacheable")
	}
	if isCacheableOpenAIRequest(&OpenAIRequest{Stream: true}, false) {
		t.Fatalf("streaming must not be cacheable")
	}
	if isCacheableOpenAIRequest(&OpenAIRequest{}, true) {
		t.Fatalf("thinking must not be cacheable")
	}
	if isCacheableOpenAIRequest(&OpenAIRequest{Tools: []OpenAITool{{Type: "function"}}}, false) {
		t.Fatalf("tool requests must not be cacheable")
	}
}
