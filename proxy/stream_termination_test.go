package proxy

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// setupMidStreamFailureHandler wires a Handler against an upstream that emits one
// good event frame, flushes it, and then writes a TRUNCATED frame. That is the
// real mid-stream failure shape: the client has already received content when the
// upstream dies, so the proxy cannot retry on another account (bytes are already
// on the wire) and must instead terminate the stream properly.
func setupMidStreamFailureHandler(t *testing.T, text string) *Handler {
	t.Helper()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "only",
		Enabled:     true,
		AccessToken: "tok",
		ProfileArn:  "arn:aws:codewhisperer:profile/only",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": text,
		}))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncated frame: declares 200 bytes, supplies 5.
		truncated := make([]byte, 12)
		binary.BigEndian.PutUint32(truncated[0:4], 200)
		binary.BigEndian.PutUint32(truncated[4:8], 20)
		_, _ = w.Write(append(truncated, 1, 2, 3, 4, 5))
	}))
	t.Cleanup(server.Close)

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}})
	t.Cleanup(func() { kiroHttpStore.Store(oldClient) })

	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
}

func midStreamPayload() *KiroPayload {
	p := &KiroPayload{}
	p.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello", ModelID: "claude-opus-4.6", Origin: "AI_EDITOR",
	}
	return p
}

// Long enough to clear the streaming flush threshold, so a content block is
// actually opened before the upstream dies.
const midStreamLongText = "This is a long partial answer that definitely exceeds the fifty rune flush threshold used by the streaming text buffer."

// On the Anthropic SSE route, a mid-stream upstream failure used to emit an
// `error` event and return immediately — leaving the opened content_block with no
// content_block_stop and the message with no message_stop. Strict SSE consumers
// wait for those terminators, so the client hangs on a half-open message instead
// of surfacing the failure.
func TestClaudeStreamTerminatesBlocksOnMidStreamFailure(t *testing.T) {
	h := setupMidStreamFailureHandler(t, midStreamLongText)

	rec := httptest.NewRecorder()
	h.handleClaudeStream(rec, midStreamPayload(), "claude-opus-4.6", false,
		claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "", nil, false)

	body := rec.Body.String()
	starts := strings.Count(body, `"type":"content_block_start"`)
	stops := strings.Count(body, `"type":"content_block_stop"`)

	if starts == 0 {
		t.Skip("upstream failed before any content block opened; nothing to terminate")
	}
	if stops != starts {
		t.Fatalf("unterminated content block(s): %d start(s) vs %d stop(s)\nbody:\n%s", starts, stops, body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("stream ended without message_stop after a mid-stream failure\nbody:\n%s", body)
	}
	// The failure itself must still be reported, not silently swallowed.
	if !strings.Contains(body, "event: error") {
		t.Fatalf("mid-stream failure was not reported to the client\nbody:\n%s", body)
	}
}

// Same defect on the OpenAI-compatible route: it returned with no error chunk, no
// finish_reason, and no [DONE], so the client saw the connection simply stop
// mid-answer and could not distinguish truncation from completion.
func TestOpenAIStreamTerminatesOnMidStreamFailure(t *testing.T) {
	h := setupMidStreamFailureHandler(t, midStreamLongText)

	rec := httptest.NewRecorder()
	h.handleOpenAIStream(rec, midStreamPayload(), "claude-opus-4.6", false, 5, "", nil, false)

	body := rec.Body.String()
	if !strings.Contains(body, "long partial answer") {
		t.Skip("upstream failed before any content reached the client")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("OpenAI stream ended without [DONE] after a mid-stream failure\nbody:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason"`) {
		t.Fatalf("OpenAI stream ended without any finish_reason\nbody:\n%s", body)
	}
}
