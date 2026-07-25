package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A Handler backed by a trace store must persist through the store (append-only
// JSONL), NOT through the legacy whole-file rewrite. This pins the Phase 2 swap:
// the old writer re-serialised all 500 retained records per request through one
// shared temp path.
func TestHandlerWithTraceStorePersistsAsJSONL(t *testing.T) {
	dir := t.TempDir()
	store := newTraceStore(filepath.Join(dir, "traces"), 64)
	defer store.Close()

	h := &Handler{traceStore: store}
	h.appendRequestLog(RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  "claude",
		API:       "claude",
		Status:    "success",
		Outcome:   outcomeSuccess,
		RequestID: "trc_wiring_1",
	})
	store.Flush()

	files, err := filepath.Glob(filepath.Join(dir, "traces", "index-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one index file, got %v (err=%v)", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rec RequestLog
	line := strings.TrimSpace(string(raw))
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("stored line is not valid JSON: %q", line)
	}
	if rec.RequestID != "trc_wiring_1" {
		t.Fatalf("RequestID = %q", rec.RequestID)
	}

	// The legacy file must NOT be produced when a store is present.
	if _, err := os.Stat(filepath.Join(dir, "request_logs.json")); err == nil {
		t.Fatal("legacy whole-file writer ran despite a trace store being configured")
	}

	// The live ring still serves the admin view.
	if logs := h.getRequestLogs(); len(logs) != 1 {
		t.Fatalf("live ring has %d records, want 1", len(logs))
	}
}

// History must survive a restart, and a pre-upgrade request_logs.json must be
// imported exactly once so upgrading does not look like data loss.
func TestHandlerReloadsHistoryAndMigratesLegacyFile(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// Pre-upgrade file in the legacy location/shape.
	if err := os.MkdirAll("data", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := []RequestLog{{Time: 1, Endpoint: "openai", Status: "success", Tokens: 5}}
	rawLegacy, _ := json.Marshal(legacy)
	if err := os.WriteFile(requestLogsPath, rawLegacy, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	// First boot: store writes one new record.
	store := newTraceStore(tracesDir(), 64)
	h := &Handler{traceStore: store}
	h.loadRequestLogs()
	h.appendRequestLog(RequestLog{Time: 2, Endpoint: "claude", Status: "error", ErrorType: "quota", RequestID: "trc_a"})
	store.Flush()
	store.Close()

	// The legacy file must have been consumed exactly once.
	if _, err := os.Stat(requestLogsPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file still present after migration (err=%v)", err)
	}
	if _, err := os.Stat(requestLogsPath + ".migrated"); err != nil {
		t.Fatalf("expected .migrated marker: %v", err)
	}

	// Second boot: both the migrated legacy record and the JSONL record load.
	store2 := newTraceStore(tracesDir(), 64)
	defer store2.Close()
	h2 := &Handler{traceStore: store2}
	h2.loadRequestLogs()
	logs := h2.getRequestLogs()
	if len(logs) != 2 {
		t.Fatalf("expected 2 reloaded records (1 legacy + 1 jsonl), got %d: %#v", len(logs), logs)
	}
	// getRequestLogs returns newest first.
	if logs[0].RequestID != "trc_a" {
		t.Fatalf("newest record = %#v", logs[0])
	}
}
