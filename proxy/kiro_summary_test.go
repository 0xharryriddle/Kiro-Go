package proxy

import (
	"strings"
	"testing"
)

func TestSummarizeKiroPayloadMasksSecrets(t *testing.T) {
	p := &KiroPayload{}
	p.ConversationState.ConversationID = "0b17e233-c915-581d-9267-8ea32873773d"
	p.ConversationState.AgentTaskType = "vibe"
	p.ConversationState.ChatTriggerType = "MANUAL"
	uim := &p.ConversationState.CurrentMessage.UserInputMessage
	uim.ModelID = "claude-opus-4.8"
	uim.Content = `<system-reminder> "ANTHROPIC_AUTH_TOKEN": "sk-ff745fb799f5cc4533979f7c4b303aa9fd64f13ec71ee1fb0644f619c39cd592", base url follows`

	got := summarizeKiroPayload(p)

	if strings.Contains(got, "sk-ff745fb7") {
		t.Fatalf("summary leaked API token: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected secret to be redacted, got: %s", got)
	}
	if !strings.Contains(got, "conv=0b17e233") {
		t.Fatalf("expected truncated conversation id, got: %s", got)
	}
	if !strings.Contains(got, "model=claude-opus-4.8") {
		t.Fatalf("expected model in summary, got: %s", got)
	}
}

func TestSummarizeKiroPayloadPreviewIsBounded(t *testing.T) {
	p := &KiroPayload{}
	uim := &p.ConversationState.CurrentMessage.UserInputMessage
	uim.Content = strings.Repeat("a", 5000)

	got := summarizeKiroPayload(p)

	if !strings.Contains(got, "contentChars=5000") {
		t.Fatalf("expected full content length reported, got: %s", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected truncation marker for long content, got: %s", got)
	}
	// The whole summary line must stay small regardless of payload size.
	if len(got) > 600 {
		t.Fatalf("summary line too long (%d bytes): %s", len(got), got)
	}
}

func TestSummarizeKiroPayloadHandlesMultibyteContent(t *testing.T) {
	p := &KiroPayload{}
	uim := &p.ConversationState.CurrentMessage.UserInputMessage
	// 300 Vietnamese multibyte runes; byte-slicing at 200 would split a rune.
	uim.Content = strings.Repeat("ề", 300)

	got := summarizeKiroPayload(p)

	// A broken UTF-8 boundary would surface as the replacement char U+FFFD.
	if strings.ContainsRune(got, '�') {
		t.Fatalf("preview split a multibyte rune: %s", got)
	}
}

func TestSummarizeKiroPayloadNil(t *testing.T) {
	if got := summarizeKiroPayload(nil); got != "<nil>" {
		t.Fatalf("expected <nil>, got: %s", got)
	}
}
