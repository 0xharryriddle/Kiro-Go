package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// Anthropic's tool_result carries `is_error: true` when a tool failed. That flag
// appeared NOWHERE in proxy/ — the converted upstream tool result hardcoded
// Status: "success" on both routes — so a failed tool was indistinguishable from
// a successful one by the time the model saw it.
//
// The practical harm is worst exactly where flagship models are strongest: an
// agentic loop that calls a tool, gets "command not found" or a stack trace, and
// is told the call SUCCEEDED will treat the error text as valid output and build
// on it instead of retrying or repairing.
//
// Why the failure is signalled in the content text rather than the Status field:
// the set of status values Kiro's upstream accepts is not documented in this
// repository and could not be verified, so emitting an unverified enum value
// risks 400-ing every failed tool turn. The content is free text and already
// carries out-of-band information by established precedent
// (toolResultImagePlaceholder), so it is the safe channel.
func TestFailedToolResultIsMarkedAsErrorClaudeRoute(t *testing.T) {
	body := `{
	  "model": "claude-opus-5",
	  "max_tokens": 1024,
	  "messages": [
	    {"role":"user","content":"run the build"},
	    {"role":"assistant","content":[
	      {"type":"tool_use","id":"toolu_1","name":"bash","input":{"cmd":"make"}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","is_error":true,
	       "content":"make: *** No rule to make target. Stop."}]}
	  ]
	}`

	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	payload := ClaudeToKiro(&req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) == 0 {
		t.Fatalf("expected structured tool results, got %#v", cur.UserInputMessageContext)
	}

	tr := cur.UserInputMessageContext.ToolResults[0]
	var text string
	for _, c := range tr.Content {
		text += c.Text
	}

	if !strings.Contains(strings.ToLower(text), "error") {
		t.Fatalf("failed tool_result carries no error signal at all; the model cannot tell it failed.\nstatus=%q content=%q", tr.Status, text)
	}
	// The original tool output must be preserved alongside the marker.
	if !strings.Contains(text, "No rule to make target") {
		t.Fatalf("original tool output was lost: %q", text)
	}
}

// A SUCCESSFUL tool result must be untouched — no error marker, byte-identical
// content. This is the guard against over-correcting.
func TestSuccessfulToolResultIsNotMarked(t *testing.T) {
	body := `{
	  "model": "claude-opus-5",
	  "max_tokens": 1024,
	  "messages": [
	    {"role":"user","content":"read it"},
	    {"role":"assistant","content":[
	      {"type":"tool_use","id":"toolu_1","name":"read","input":{}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":"file contents here"}]}
	  ]
	}`

	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	payload := ClaudeToKiro(&req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) == 0 {
		t.Fatalf("expected structured tool results")
	}
	tr := cur.UserInputMessageContext.ToolResults[0]
	var text string
	for _, c := range tr.Content {
		text += c.Text
	}
	if text != "file contents here" {
		t.Fatalf("successful tool result content was modified: %q", text)
	}
	if tr.Status != "success" {
		t.Fatalf("successful tool result status changed to %q", tr.Status)
	}
}

// is_error:false must behave exactly like an absent flag.
func TestExplicitlyNonErrorToolResultIsNotMarked(t *testing.T) {
	body := `{
	  "model": "claude-opus-5",
	  "max_tokens": 1024,
	  "messages": [
	    {"role":"user","content":"read it"},
	    {"role":"assistant","content":[
	      {"type":"tool_use","id":"toolu_1","name":"read","input":{}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"all good"}]}
	  ]
	}`

	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	payload := ClaudeToKiro(&req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	tr := cur.UserInputMessageContext.ToolResults[0]
	var text string
	for _, c := range tr.Content {
		text += c.Text
	}
	if text != "all good" {
		t.Fatalf("is_error:false result was modified: %q", text)
	}
}
