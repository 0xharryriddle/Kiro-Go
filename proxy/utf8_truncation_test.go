package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Every truncation in the request path used raw byte slicing (s[:n]) on UTF-8
// text. Slicing at an arbitrary byte offset splits a multi-byte rune and emits
// the replacement character (or an outright invalid sequence) at the cut point.
//
// This is not cosmetic for non-ASCII users: the tool description, the current
// message, and the tool-result continuation are all sent upstream verbatim, so a
// Vietnamese/Chinese/emoji-bearing request ships malformed UTF-8 to the model.
// maxToolDescLen is 10237 — an ODD number — so a 2-byte rune sequence is
// guaranteed to be split there.

func TestToolDescriptionTruncationKeepsValidUTF8(t *testing.T) {
	// 6000 x "é" = 12000 bytes, over the 10237 limit, and every rune is 2 bytes
	// so the byte cut lands mid-rune.
	desc := strings.Repeat("é", 6000)
	tools, _ := convertClaudeTools([]ClaudeTool{{
		Name:        "read_file",
		Description: desc,
		InputSchema: map[string]interface{}{"type": "object"},
	}})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	got := tools[0].ToolSpecification.Description
	if !utf8.ValidString(got) {
		t.Fatalf("tool description truncated mid-rune: invalid UTF-8 (len=%d)", len(got))
	}
}

func TestOpenAIToolDescriptionTruncationKeepsValidUTF8(t *testing.T) {
	desc := strings.Repeat("é", 6000)
	tool := OpenAITool{Type: "function"}
	tool.Function.Name = "read_file"
	tool.Function.Description = desc
	tool.Function.Parameters = map[string]interface{}{"type": "object"}

	tools, _ := convertOpenAITools([]OpenAITool{tool})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	got := tools[0].ToolSpecification.Description
	if !utf8.ValidString(got) {
		t.Fatalf("OpenAI tool description truncated mid-rune: invalid UTF-8 (len=%d)", len(got))
	}
}

func TestToolResultsContinuationKeepsValidUTF8(t *testing.T) {
	// 4000 x "é" = 8000 bytes, over the 4000-byte cap.
	big := strings.Repeat("é", 4000)
	out := buildToolResultsContinuation([]KiroToolResult{{
		ToolUseID: "t1",
		Content:   []KiroResultContent{{Text: big}},
		Status:    "success",
	}})
	if !utf8.ValidString(out) {
		t.Fatalf("tool-result continuation truncated mid-rune: invalid UTF-8 (len=%d)", len(out))
	}
}

func TestCurrentMessageTruncationKeepsValidUTF8(t *testing.T) {
	// A single current message far over maxPayloadBytes forces
	// truncateCurrentMessage to cut at a byte budget.
	huge := strings.Repeat("é", 700_000) // ~1.4MB
	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 1024,
		Messages:  []ClaudeMessage{{Role: "user", Content: huge}},
	}
	payload := ClaudeToKiro(req, false)
	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !utf8.ValidString(got) {
		t.Fatalf("current message truncated mid-rune: invalid UTF-8 sent upstream (len=%d)", len(got))
	}
}

// Truncation must still actually truncate: a rune-safe cut may drop at most a
// few trailing bytes, never leave the value over its limit or empty it out.
func TestRuneSafeTruncationStillBounded(t *testing.T) {
	desc := strings.Repeat("é", 6000)
	tools, _ := convertClaudeTools([]ClaudeTool{{
		Name:        "t",
		Description: desc,
		InputSchema: map[string]interface{}{"type": "object"},
	}})
	got := tools[0].ToolSpecification.Description
	// "..." is appended after the cut, so the budget applies to the prefix.
	trimmed := strings.TrimSuffix(got, "...")
	if len(trimmed) > maxToolDescLen {
		t.Fatalf("truncation exceeded its budget: %d > %d", len(trimmed), maxToolDescLen)
	}
	if len(trimmed) < maxToolDescLen-4 {
		t.Fatalf("truncation dropped far more than one rune: %d, budget %d", len(trimmed), maxToolDescLen)
	}
}

// ASCII input must be byte-identical to the previous behaviour, so this change
// cannot regress existing English-language deployments.
func TestASCIITruncationUnchanged(t *testing.T) {
	desc := strings.Repeat("a", 12000)
	tools, _ := convertClaudeTools([]ClaudeTool{{
		Name:        "t",
		Description: desc,
		InputSchema: map[string]interface{}{"type": "object"},
	}})
	got := tools[0].ToolSpecification.Description
	want := desc[:maxToolDescLen] + "..."
	if got != want {
		t.Fatalf("ASCII truncation changed: len(got)=%d len(want)=%d", len(got), len(want))
	}
}
