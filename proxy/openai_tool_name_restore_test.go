package proxy

import "testing"

// longMCPToolName is over the 64-character limit shortenToolName enforces, in
// the shape real MCP tools use (mcp__server__tool).
const longMCPToolName = "mcp__agentmemory__memory_smart_search_with_a_long_descriptive_suffix"

func openAIToolNamed(name string) OpenAITool {
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = name
	tool.Function.Description = "does a thing"
	tool.Function.Parameters = map[string]interface{}{"type": "object"}
	return tool
}

// A tool name that gets shortened for Kiro must be recoverable, or the client
// receives a tool_call naming a tool it never registered and cannot dispatch it.
//
// The Claude route always returned this map; the OpenAI route applied the same
// rewrite and returned nothing, so the shortened name reached the client as-is.
func TestOpenAIToolsReturnNameMapForShortenedNames(t *testing.T) {
	if len(longMCPToolName) <= 64 {
		t.Fatalf("fixture is not long enough to be shortened: %d chars", len(longMCPToolName))
	}

	wrappers, nameMap := convertOpenAITools([]OpenAITool{openAIToolNamed(longMCPToolName)})
	if len(wrappers) != 1 {
		t.Fatalf("expected 1 wrapper, got %d", len(wrappers))
	}

	sent := wrappers[0].ToolSpecification.Name
	if sent == longMCPToolName {
		t.Fatalf("precondition: name was not shortened, so this test proves nothing")
	}
	if nameMap == nil {
		t.Fatal("no name map returned — the shortened name cannot be restored")
	}
	if got := nameMap[sent]; got != longMCPToolName {
		t.Fatalf("nameMap[%q] = %q, want %q", sent, got, longMCPToolName)
	}
}

// Names that pass through unchanged must NOT populate the map: a needless entry
// would be carried on every payload for no benefit.
func TestOpenAIToolsOmitNameMapWhenNothingRewritten(t *testing.T) {
	_, nameMap := convertOpenAITools([]OpenAITool{
		openAIToolNamed("read_file"),
		openAIToolNamed("search_files"),
	})
	if nameMap != nil {
		t.Fatalf("expected no name map for unmodified names, got %v", nameMap)
	}
}

// The map must reach the payload: convertOpenAITools returning it is useless if
// OpenAIToKiro drops it, which is exactly the bug that existed.
func TestOpenAIToKiroCarriesToolNameMap(t *testing.T) {
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-opus-5",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools:    []OpenAITool{openAIToolNamed(longMCPToolName)},
	}, false)

	if payload.ToolNameMap == nil {
		t.Fatal("OpenAIToKiro dropped the tool name map")
	}
	found := false
	for sanitized, original := range payload.ToolNameMap {
		if original == longMCPToolName {
			found = true
			if sanitized == longMCPToolName {
				t.Fatalf("map entry is a no-op: %q -> %q", sanitized, original)
			}
		}
	}
	if !found {
		t.Fatalf("payload map does not restore %q: %v", longMCPToolName, payload.ToolNameMap)
	}
}

// Parity with the Claude route: the same long name must be restorable over
// either API surface. A client should not get different tool-name behaviour
// depending on which endpoint it uses.
func TestToolNameRestorationParityAcrossRoutes(t *testing.T) {
	openAIPayload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-opus-5",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools:    []OpenAITool{openAIToolNamed(longMCPToolName)},
	}, false)

	claudePayload := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-opus-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
		Tools: []ClaudeTool{{
			Name:        longMCPToolName,
			Description: "does a thing",
			InputSchema: map[string]interface{}{"type": "object"},
		}},
	}, false)

	restores := func(m map[string]string) bool {
		for _, original := range m {
			if original == longMCPToolName {
				return true
			}
		}
		return false
	}

	openAIOK := restores(openAIPayload.ToolNameMap)
	claudeOK := restores(claudePayload.ToolNameMap)
	if openAIOK != claudeOK {
		t.Fatalf("route parity broken: openai restores=%v claude restores=%v", openAIOK, claudeOK)
	}
	if !openAIOK {
		t.Fatal("neither route restores the original tool name")
	}
}
