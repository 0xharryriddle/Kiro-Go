package proxy

import (
	"testing"
)

// When the max_uses budget runs out partway through a round that carries several
// web_search uses, EVERY unexecuted one needs its own error result. The budget
// cannot recover mid-round, so a single error followed by empty successes would
// tell the model "one search was blocked, the rest found nothing" — which is a
// different and wrong statement about the remaining searches.
func TestFlushContentMarksEverySkippedSearchInARound(t *testing.T) {
	toolUses := []KiroToolUse{
		{ToolUseID: "t1", Name: webSearchToolName, Input: map[string]interface{}{"query": "first"}},
		{ToolUseID: "t2", Name: webSearchToolName, Input: map[string]interface{}{"query": "second"}},
		{ToolUseID: "t3", Name: webSearchToolName, Input: map[string]interface{}{"query": "third"}},
	}
	// The first search ran; the budget then ran out, so #2 and #3 are skipped.
	executed := &WebSearchResults{Results: []WebSearchResult{{Title: "hit", URL: "https://e.test"}}}
	searched := []*WebSearchResults{executed, nil, nil}
	skipped := map[int]bool{1: true, 2: true}

	content := buildFlushContent(nil, "", toolUses, searched, skipped)

	var results []interface{}
	for _, block := range content {
		if block["type"] == "web_search_tool_result" {
			results = append(results, block["content"])
		}
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 web_search_tool_result blocks, got %d", len(results))
	}

	// #1 executed: an ARRAY of results.
	first, ok := results[0].([]map[string]interface{})
	if !ok || len(first) != 1 {
		t.Fatalf("executed search should carry a result array, got %#v", results[0])
	}

	// #2 and #3 skipped: each an error OBJECT naming the budget.
	for i, idx := range []int{1, 2} {
		errObj, ok := results[idx].(map[string]interface{})
		if !ok {
			t.Fatalf("skipped search %d rendered as %T, want an error object", i+2, results[idx])
		}
		if errObj["type"] != "web_search_tool_result_error" {
			t.Fatalf("skipped search %d type = %v", i+2, errObj["type"])
		}
		if errObj["error_code"] != webSearchErrorMaxUsesExceeded {
			t.Fatalf("skipped search %d error_code = %v, want %q",
				i+2, errObj["error_code"], webSearchErrorMaxUsesExceeded)
		}
	}
}

// Client (non-web_search) tool uses in the same round must be unaffected by the
// skipped bookkeeping: they are passed through as ordinary tool_use blocks, and
// their indices must not be misread as skipped searches.
func TestFlushContentSkippedBookkeepingIgnoresClientTools(t *testing.T) {
	toolUses := []KiroToolUse{
		{ToolUseID: "t1", Name: "Bash", Input: map[string]interface{}{"command": "ls"}},
		{ToolUseID: "t2", Name: webSearchToolName, Input: map[string]interface{}{"query": "q"}},
	}
	content := buildFlushContent(nil, "", toolUses, []*WebSearchResults{nil, nil}, map[int]bool{1: true})

	var sawClientTool bool
	for _, block := range content {
		if block["type"] == "tool_use" {
			if block["name"] != "Bash" {
				t.Fatalf("unexpected raw tool_use %v", block["name"])
			}
			sawClientTool = true
		}
		if block["type"] == "web_search_tool_result" {
			errObj, ok := block["content"].(map[string]interface{})
			if !ok || errObj["error_code"] != webSearchErrorMaxUsesExceeded {
				t.Fatalf("the skipped search at index 1 should be an error object, got %#v", block["content"])
			}
		}
	}
	if !sawClientTool {
		t.Fatal("client tool_use was dropped from the flush")
	}
}
