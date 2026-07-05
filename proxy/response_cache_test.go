package proxy

import "testing"

func TestResponseCacheKey_StableAndNamespaced(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	k1 := responseCacheKey("openai", body)
	k2 := responseCacheKey("openai", body)
	if k1 != k2 {
		t.Fatalf("expected stable key, got %q vs %q", k1, k2)
	}
	// Different endpoint namespace -> different key even for identical bodies.
	if responseCacheKey("claude", body) == k1 {
		t.Fatalf("expected endpoint namespace to change the key")
	}
	// Different body -> different key.
	if responseCacheKey("openai", []byte(`{"model":"m2"}`)) == k1 {
		t.Fatalf("expected body difference to change the key")
	}
}

func TestResponseCacheKey_EmptyForNoInput(t *testing.T) {
	// A key is always derived (sha256 of endpoint+body); empty body still yields
	// a deterministic non-empty key.
	if responseCacheKey("openai", nil) == "" {
		t.Fatalf("expected non-empty key even for nil body")
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
	c.Set("", []byte("x"), 100, 1000)         // empty key
	c.Set("k", nil, 100, 1000)                // empty body
	c.Set("k2", []byte("x"), 0, 1000)         // non-positive TTL
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
