package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// decodeClaudeRequest round-trips a wire body through the real request type, so
// these tests exercise exactly what a client's JSON produces after decoding
// rather than a hand-built struct that could paper over a dropped field.
func decodeClaudeRequest(t *testing.T, body string) *ClaudeRequest {
	t.Helper()
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return &req
}

// buildToolCachedRequest constructs a request carrying a realistically sized
// agentic tool set, with the cache_control marker on the FINAL tool so the
// cached prefix spans every schema. It goes through the JSON wire format on
// purpose: a hand-built struct would bypass decoding and could not detect a
// dropped `cache_control` field.
func buildToolCachedRequest(t *testing.T, model string, toolCount int) *ClaudeRequest {
	t.Helper()

	tools := make([]map[string]interface{}, 0, toolCount)
	for i := 0; i < toolCount; i++ {
		tool := map[string]interface{}{
			"name": fmt.Sprintf("tool_%02d", i),
			"description": fmt.Sprintf(
				"Tool %02d. %s", i,
				strings.Repeat("Performs a well-specified operation on the workspace and returns a structured result. ", 12),
			),
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string", "description": strings.Repeat("absolute path to operate on. ", 6)},
					"content": map[string]interface{}{"type": "string", "description": strings.Repeat("payload body to apply. ", 6)},
					"limit":   map[string]interface{}{"type": "integer", "description": "maximum number of results"},
				},
				"required": []string{"path"},
			},
		}
		if i == toolCount-1 {
			tool["cache_control"] = map[string]interface{}{"type": "ephemeral"}
		}
		tools = append(tools, tool)
	}

	body, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"max_tokens": 1024,
		"tools":      tools,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return decodeClaudeRequest(t, string(body))
}

// Tool definitions are the largest STABLE prefix in an agentic request: a
// coding agent ships the same 15-20 tool schemas on every single turn. Anthropic
// prompt caching is keyed on that prefix, so dropping cache_control from a tool
// definition forfeits the biggest available saving on every request.
func TestToolCacheControlSurvivesDecoding(t *testing.T) {
	req := decodeClaudeRequest(t, `{
	  "model": "claude-opus-5",
	  "max_tokens": 1024,
	  "tools": [
	    {"name":"read_file","description":"reads a file","input_schema":{"type":"object"},
	     "cache_control":{"type":"ephemeral"}}
	  ],
	  "messages": [{"role":"user","content":"hi"}]
	}`)

	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	if req.Tools[0].CacheControl == nil {
		t.Fatal("tool cache_control was dropped during decoding: prompt caching can never apply to tool definitions")
	}
	if got := extractPromptCacheTTL(req.Tools[0]); got != defaultPromptCacheTTL {
		t.Fatalf("extractPromptCacheTTL(tool) = %v, want %v", got, defaultPromptCacheTTL)
	}
}

// A 1h cache_control on a tool must be honoured, not silently downgraded.
func TestToolCacheControlHonoursOneHourTTL(t *testing.T) {
	req := decodeClaudeRequest(t, `{
	  "model": "claude-opus-5",
	  "tools": [
	    {"name":"t","description":"d","input_schema":{"type":"object"},
	     "cache_control":{"type":"ephemeral","ttl":"1h"}}
	  ],
	  "messages": [{"role":"user","content":"hi"}]
	}`)
	if got := extractPromptCacheTTL(req.Tools[0]); got != time.Hour {
		t.Fatalf("tool 1h TTL = %v, want 1h", got)
	}
}

// cache_control on a structured content block must survive decoding too, so a
// long user document marked cacheable actually becomes a breakpoint.
func TestContentBlockCacheControlSurvivesDecoding(t *testing.T) {
	req := decodeClaudeRequest(t, `{
	  "model": "claude-opus-5",
	  "messages": [
	    {"role":"user","content":[
	      {"type":"text","text":"a long document","cache_control":{"type":"ephemeral"}}
	    ]}
	  ]
	}`)

	blocks, ok := req.Messages[0].Content.([]interface{})
	if !ok {
		t.Fatalf("expected structured content, got %T", req.Messages[0].Content)
	}
	if got := extractPromptCacheTTL(blocks[0]); got != defaultPromptCacheTTL {
		t.Fatalf("content block cache TTL = %v, want %v", got, defaultPromptCacheTTL)
	}
}

// End-to-end: a request whose ONLY cache_control sits on the tools must still
// produce a cache breakpoint. Before the fix the tool marker was dropped, so no
// breakpoint existed and the tracker reported zero cache activity forever.
func TestToolOnlyCacheControlProducesBreakpoint(t *testing.T) {
	// A single toy schema is only a few dozen tokens, which is legitimately
	// below the minimum cacheable prefix length, so the tracker would discard
	// it and the test would go red for the wrong reason. Build a realistically
	// sized agentic tool set (the marker sits on the LAST tool, which is how
	// clients cache the whole tool prefix) so the breakpoint clears the floor.
	req := buildToolCachedRequest(t, "claude-opus-5", 18)

	tracker := newPromptCacheTracker(defaultPromptCacheTTL)
	profile := tracker.BuildClaudeProfile(req, 20000)
	if profile == nil {
		t.Fatal("no cache profile built: tool cache_control produced no breakpoint")
	}
	if len(profile.Breakpoints) == 0 {
		t.Fatal("expected at least one breakpoint from the tool marker")
	}

	// First call creates, second call reads: that is the whole point of caching.
	first := tracker.Compute("acct-1", profile)
	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)

	if first.CacheReadInputTokens != 0 {
		t.Fatalf("first call must not report a cache read, got %d", first.CacheReadInputTokens)
	}
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("second identical call must report a cache read, got %d", second.CacheReadInputTokens)
	}
}
