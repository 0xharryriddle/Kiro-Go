package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// initTraceConfig writes a config file with the given trace-capture settings and
// loads it, mirroring how the rest of the proxy tests bootstrap config.
func initTraceConfig(t *testing.T, mode string, acknowledgeRisk bool) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	raw := map[string]interface{}{
		"password": "test",
		"port":     8080,
	}
	if mode != "" {
		raw["traceCaptureMode"] = mode
	}
	if acknowledgeRisk {
		raw["traceCaptureAcknowledgeRisk"] = true
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgFile, encoded, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

// newCaptureHandler builds a Handler wired to a temp body store, with the live
// ring only (no background goroutines).
func newCaptureHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	return &Handler{traceBodies: newTraceBodyStore(dir)}, dir
}

// The default posture must never write prompt text to disk. This is the single
// most important guarantee in the body-capture feature: upgrading Kiro-Go must
// not silently start retaining user prompts.
func TestEmitTraceStoresNoBodiesInDefaultMetaMode(t *testing.T) {
	initTraceConfig(t, "", false)
	h, dir := newCaptureHandler(t)

	tr := newTraceRecorder("claude", "sonnet", false, "key-1")
	tr.noteRequestPayload(&KiroPayload{ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/X"})
	tr.noteResponseText("the assistant replied with something private")
	h.emitTrace(tr, outcomeSuccess, 200)

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected one record, got %d", len(logs))
	}
	if logs[0].BodyRef != "" {
		t.Fatalf("meta mode must not store bodies, got ref %q", logs[0].BodyRef)
	}
	// Nothing at all should have been written under the body root.
	var found []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("meta mode wrote body files: %v", found)
	}
}

// In redacted mode bodies are stored, reachable by ref, and PII-redacted.
func TestEmitTraceStoresRedactedBodies(t *testing.T) {
	initTraceConfig(t, config.TraceCaptureRedacted, false)
	h, _ := newCaptureHandler(t)

	tr := newTraceRecorder("claude", "sonnet", false, "key-1")
	tr.noteRequestPayload(&KiroPayload{ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/X"})
	tr.noteResponseText("contact me at alice@example.com please")
	h.emitTrace(tr, outcomeSuccess, 200)

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected one record, got %d", len(logs))
	}
	ref := logs[0].BodyRef
	if ref == "" {
		t.Fatal("redacted mode must store bodies and set BodyRef")
	}
	if filepath.IsAbs(ref) {
		t.Fatalf("BodyRef must be relative, got %q", ref)
	}

	body, err := h.traceBodies.Read(ref)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body.Request), "profile/X") {
		t.Fatalf("request body not captured: %q", body.Request)
	}
	// redactPII must have been applied to the response.
	if strings.Contains(string(body.Response), "alice@example.com") {
		t.Fatalf("redacted mode leaked an email address: %q", body.Response)
	}
}

// "off" disables trace body capture entirely.
func TestEmitTraceStoresNoBodiesWhenCaptureOff(t *testing.T) {
	initTraceConfig(t, config.TraceCaptureOff, false)
	h, _ := newCaptureHandler(t)

	tr := newTraceRecorder("openai", "gpt", false, "")
	tr.noteRequestPayload(&KiroPayload{ProfileArn: "arn:x"})
	tr.noteResponseText("secret text")
	h.emitTrace(tr, outcomeSuccess, 200)

	logs := h.getRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("expected one record, got %d", len(logs))
	}
	if logs[0].BodyRef != "" {
		t.Fatalf("off mode must not store bodies, got %q", logs[0].BodyRef)
	}
}

// "full" without the explicit risk acknowledgement must degrade to redacted, so
// a single config typo cannot start retaining verbatim prompts.
func TestFullModeWithoutAcknowledgementStillRedacts(t *testing.T) {
	initTraceConfig(t, config.TraceCaptureFull, false)
	if got := config.GetTraceCaptureMode(); got != config.TraceCaptureRedacted {
		t.Fatalf("mode = %q, want degrade to redacted", got)
	}

	h, _ := newCaptureHandler(t)
	tr := newTraceRecorder("claude", "sonnet", false, "")
	tr.noteResponseText("reach me at bob@example.com")
	h.emitTrace(tr, outcomeSuccess, 200)

	logs := h.getRequestLogs()
	ref := logs[0].BodyRef
	if ref == "" {
		t.Fatal("degraded mode should still capture (as redacted)")
	}
	body, err := h.traceBodies.Read(ref)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body.Response), "bob@example.com") {
		t.Fatalf("degraded full mode must redact PII, got %q", body.Response)
	}
}

// With the acknowledgement, full mode keeps prompt text verbatim but must STILL
// scrub credentials: "full" governs prompt retention, never secret retention.
func TestFullModeWithAcknowledgementKeepsTextButScrubsSecrets(t *testing.T) {
	initTraceConfig(t, config.TraceCaptureFull, true)
	if got := config.GetTraceCaptureMode(); got != config.TraceCaptureFull {
		t.Fatalf("mode = %q, want full", got)
	}

	h, _ := newCaptureHandler(t)
	tr := newTraceRecorder("claude", "sonnet", false, "")
	tr.noteResponseText(`user alice@example.com key ksk_live_supersecret done`)
	h.emitTrace(tr, outcomeSuccess, 200)

	logs := h.getRequestLogs()
	ref := logs[0].BodyRef
	if ref == "" {
		t.Fatal("full mode must capture bodies")
	}
	body, err := h.traceBodies.Read(ref)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body.Response)
	// Verbatim prompt text is retained in full mode...
	if !strings.Contains(got, "alice@example.com") {
		t.Fatalf("full mode should retain text verbatim, got %q", got)
	}
	// ...but credentials are scrubbed regardless of mode.
	if strings.Contains(got, "ksk_live_supersecret") {
		t.Fatalf("full mode leaked a credential: %q", got)
	}
}
