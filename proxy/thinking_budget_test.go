package proxy

import (
	"strings"
	"testing"
)

// A client that asks for extended thinking sends thinking.budget_tokens. Kiro's
// upstream payload has no budget field (InferenceConfig carries only maxTokens,
// temperature, topP), so the ONLY channel that can convey a thinking budget is
// the <max_thinking_length> directive in the system prompt.
//
// That directive was hardcoded to 200000, so budget_tokens was validated and
// then discarded: a client asking for a 64000-token budget and a client asking
// for 1024 produced byte-identical upstream requests. These tests pin the
// budget to the directive that actually ships.
func TestThinkingBudgetReachesUpstreamDirective(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 128000,
		Thinking:  &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 64000},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
	}

	payload := ClaudeToKiro(req, true)
	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content

	// The system prompt is what carries the directive; locate it wherever the
	// converter places it.
	all := got + " " + payloadSystemText(t, payload)
	if !strings.Contains(all, "<max_thinking_length>64000</max_thinking_length>") {
		t.Fatalf("client thinking budget 64000 never reached the upstream directive.\ngot: %s", firstN(all, 400))
	}
	if strings.Contains(all, "<max_thinking_length>200000</max_thinking_length>") {
		t.Fatalf("hardcoded 200000 directive still present despite an explicit client budget")
	}
}

// With no explicit budget (thinking enabled via the -thinking model suffix, or
// adaptive mode) the previous default must be preserved exactly, so existing
// deployments see no behavioural change.
func TestThinkingWithoutBudgetKeepsDefaultDirective(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 8192,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
	}

	payload := ClaudeToKiro(req, true)
	all := payloadSystemText(t, payload)
	if !strings.Contains(all, "<max_thinking_length>200000</max_thinking_length>") {
		t.Fatalf("default directive changed when no budget was supplied.\ngot: %s", firstN(all, 400))
	}
}

// A budget must not leak into a non-thinking request.
func TestNoThinkingDirectiveWhenThinkingDisabled(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 8192,
		Thinking:  &ClaudeThinkingConfig{Type: "disabled"},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
	}

	payload := ClaudeToKiro(req, false)
	all := payloadSystemText(t, payload)
	if strings.Contains(all, "max_thinking_length") {
		t.Fatalf("thinking directive leaked into a non-thinking request: %s", firstN(all, 400))
	}
}

// The token estimate and cache profile are built from the thinking-adjusted
// request, so prependThinkingSystem must carry the same budget the upstream
// payload uses; otherwise the estimate is computed against different bytes.
func TestPrependThinkingSystemCarriesBudget(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 128000,
		Thinking:  &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 32000},
		System:    "base system",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok || len(blocks) == 0 {
		t.Fatalf("expected structured system blocks, got %T", cloned.System)
	}
	first, ok := blocks[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a map block, got %T", blocks[0])
	}
	text, _ := first["text"].(string)
	if !strings.Contains(text, "<max_thinking_length>32000</max_thinking_length>") {
		t.Fatalf("prependThinkingSystem dropped the budget: %q", text)
	}
}

// payloadSystemText returns every piece of instruction text the payload carries,
// so a test does not have to assume WHERE the converter puts the system prompt.
// ClaudeToKiro currently primes it as the first history turn; joining all of it
// keeps these assertions valid if that placement changes.
func payloadSystemText(t *testing.T, payload *KiroPayload) string {
	t.Helper()
	if payload == nil {
		t.Fatal("nil payload")
	}
	var sb strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			sb.WriteString(h.UserInputMessage.Content)
			sb.WriteString("\n")
		}
		if h.AssistantResponseMessage != nil {
			sb.WriteString(h.AssistantResponseMessage.Content)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	return sb.String()
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
