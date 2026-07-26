package proxy

// TEMPORARY adversarial-review probe. Deleted before the review finishes.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 1. Does estimateContentBlocksTokens survive an OBJECT content (no type
// assertion on []map, no panic) and still produce a sane number?
func TestTmpEstimateTokensWithObjectContent(t *testing.T) {
	toolUses := []KiroToolUse{
		{ToolUseID: "t1", Name: webSearchToolName, Input: map[string]interface{}{"query": "q"}},
	}
	skippedContent := buildFlushContent(nil, "", toolUses, []*WebSearchResults{nil}, map[int]bool{0: true})
	okContent := buildFlushContent(nil, "", toolUses, []*WebSearchResults{nil}, nil)

	nSkip := estimateContentBlocksTokens(skippedContent)
	nOK := estimateContentBlocksTokens(okContent)
	t.Logf("tokens skipped=%d ok=%d", nSkip, nOK)
	if nSkip < 1 {
		t.Fatalf("skipped content produced %d tokens", nSkip)
	}

	// 2. JSON wire shape of the skipped block.
	b, err := json.Marshal(skippedContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("JSON: %s", b)
	if !strings.Contains(string(b), `"content":{"error_code":"max_uses_exceeded","type":"web_search_tool_result_error"}`) {
		t.Logf("NOTE shape: %s", b)
	}
	// Does the emitted web_search_tool_result carry tool_use_id?
	if strings.Contains(string(b), `"web_search_tool_result","tool_use_id"`) ||
		strings.Contains(string(b), `"tool_use_id"`) {
		t.Logf("tool_use_id present")
	} else {
		t.Logf("tool_use_id ABSENT on web_search_tool_result")
	}
}

// 3. Does the SSE renderer emit the object shape (and not drop/mangle it)?
func TestTmpSSEWithObjectContent(t *testing.T) {
	h := &Handler{}
	toolUses := []KiroToolUse{
		{ToolUseID: "t1", Name: webSearchToolName, Input: map[string]interface{}{"query": "q"}},
	}
	content := buildFlushContent(nil, "hello", toolUses, []*WebSearchResults{nil}, map[int]bool{0: true})
	rec := httptest.NewRecorder()
	h.renderWebSearchLoopSSE(rec, "claude-sonnet-4", content, "end_turn", 10, 5)
	body := rec.Body.String()
	t.Logf("SSE:\n%s", body)
	if !strings.Contains(body, "web_search_tool_result_error") {
		t.Fatalf("SSE dropped the error object")
	}
}

// 4. How is the envelope-mismatch error classified by the failover router?
func TestTmpEnvelopeErrorClassification(t *testing.T) {
	msgs := []string{
		`MCP response id "b" does not match request id "a"`,
		`MCP response jsonrpc version "1.0", want "2.0"`,
		`MCP response exceeds 8388608 bytes`,
	}
	for _, m := range msgs {
		t.Logf("%q auth=%v quota=%v overage=%v suspension=%v inputTooLong=%v profileAuthz=%v",
			m, isAuthErrorMessage(m), isQuotaErrorMessage(m), isOverageErrorMessage(m),
			isSuspensionErrorMessage(m), isInputTooLongErrorMessage(m), isProfileOrPlanAuthzError(m))
	}
}

// 5. Can a non-web_search index ever end up marked skipped by the loop's
// marking rule? Replicate the exact marking loop from websearch_loop.go.
func TestTmpSkippedMarkingNeverHitsClientTool(t *testing.T) {
	toolUses := []KiroToolUse{
		{ToolUseID: "a", Name: webSearchToolName},
		{ToolUseID: "b", Name: "Bash"},
		{ToolUseID: "c", Name: webSearchToolName},
	}
	maxUses, searchCount := 1, 0
	var skipped map[int]bool
	for i, tu := range toolUses {
		if tu.Name != webSearchToolName {
			continue
		}
		if searchCount >= maxUses {
			if skipped == nil {
				skipped = make(map[int]bool)
			}
			for j := i; j < len(toolUses); j++ {
				if toolUses[j].Name == webSearchToolName {
					skipped[j] = true
				}
			}
			break
		}
		searchCount++
	}
	t.Logf("skipped=%v", skipped)
	for idx := range skipped {
		if toolUses[idx].Name != webSearchToolName {
			t.Fatalf("client tool index %d marked skipped", idx)
		}
	}
	// And buildFlushContent with that skipped map must leave the client tool alone.
	content := buildFlushContent(nil, "", toolUses, make([]*WebSearchResults, 3), skipped)
	b, _ := json.Marshal(content)
	t.Logf("mixed JSON: %s", b)
}
