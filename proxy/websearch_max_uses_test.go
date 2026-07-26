package proxy

import "testing"

// When the max_uses budget is exhausted, the loop stops executing web_search but
// still renders the skipped tool use. Rendering it as a NORMAL
// web_search_tool_result with an empty content array makes it indistinguishable
// from a search that ran and genuinely found nothing — the client (and the model
// reading the next turn) cannot tell "I am out of budget" from "the web has no
// answer", and will happily conclude the latter.
//
// Anthropic's contract for this case is a web_search_tool_result whose content is
// a web_search_tool_result_error object carrying error_code "max_uses_exceeded".
func TestFlushContentMarksSkippedSearchAsMaxUsesExceeded(t *testing.T) {
	toolUses := []KiroToolUse{
		{ToolUseID: "toolu_1", Name: webSearchToolName, Input: map[string]interface{}{"query": "second"}},
	}
	// searched[0] == nil AND the index is marked skipped: the search never ran.
	content := buildFlushContent(nil, "", toolUses, []*WebSearchResults{nil}, map[int]bool{0: true})

	var resultBlock map[string]interface{}
	for _, b := range content {
		if b["type"] == "web_search_tool_result" {
			resultBlock = b
		}
	}
	if resultBlock == nil {
		t.Fatalf("no web_search_tool_result block in %#v", content)
	}

	errObj, ok := resultBlock["content"].(map[string]interface{})
	if !ok {
		t.Fatalf("skipped search rendered as a successful result (%T), not an error object: %#v",
			resultBlock["content"], resultBlock["content"])
	}
	if errObj["type"] != "web_search_tool_result_error" {
		t.Fatalf("content type = %v, want web_search_tool_result_error", errObj["type"])
	}
	if errObj["error_code"] != webSearchErrorMaxUsesExceeded {
		t.Fatalf("error_code = %v, want %q", errObj["error_code"], webSearchErrorMaxUsesExceeded)
	}
}

// A search that DID run and legitimately returned zero results must keep the
// successful empty-array shape. This is the control that stops the fix above
// from being implemented as "always report an error when there are no results".
func TestFlushContentKeepsEmptySuccessForExecutedSearch(t *testing.T) {
	toolUses := []KiroToolUse{
		{ToolUseID: "toolu_1", Name: webSearchToolName, Input: map[string]interface{}{"query": "q"}},
	}
	// Executed (not in the skipped set) but the provider returned no results.
	content := buildFlushContent(nil, "", toolUses, []*WebSearchResults{{}}, nil)

	var resultBlock map[string]interface{}
	for _, b := range content {
		if b["type"] == "web_search_tool_result" {
			resultBlock = b
		}
	}
	if resultBlock == nil {
		t.Fatalf("no web_search_tool_result block in %#v", content)
	}
	got, ok := resultBlock["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("executed search must keep the successful array shape, got %T", resultBlock["content"])
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty result array, got %#v", got)
	}
}

// A search that ran and returned results is unaffected by the skipped-set logic.
func TestFlushContentKeepsResultsForExecutedSearch(t *testing.T) {
	snippet := "body"
	results := &WebSearchResults{
		Results: []WebSearchResult{{Title: "T", URL: "https://example.test", Snippet: &snippet}},
	}
	toolUses := []KiroToolUse{
		{ToolUseID: "toolu_1", Name: webSearchToolName, Input: map[string]interface{}{"query": "q"}},
	}
	content := buildFlushContent(nil, "", toolUses, []*WebSearchResults{results}, map[int]bool{})

	for _, b := range content {
		if b["type"] != "web_search_tool_result" {
			continue
		}
		got, ok := b["content"].([]map[string]interface{})
		if !ok {
			t.Fatalf("expected the successful array shape, got %T", b["content"])
		}
		if len(got) != 1 || got[0]["title"] != "T" {
			t.Fatalf("result block lost its payload: %#v", got)
		}
		return
	}
	t.Fatal("no web_search_tool_result block emitted")
}
