package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

func newTraceSettingsHandler(t *testing.T) *Handler {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config init: %v", err)
	}
	return &Handler{}
}

func getSettings(t *testing.T, h *Handler) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	h.apiGetSettings(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	return body
}

func postSettings(t *testing.T, h *Handler, payload string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(payload))
	h.apiUpdateSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d: %s", rec.Code, rec.Body.String())
	}
}

// The default must be "meta": upgrading an existing deployment must not start
// retaining prompt text.
func TestSettingsReportsMetaCaptureByDefault(t *testing.T) {
	h := newTraceSettingsHandler(t)
	if got := getSettings(t, h)["traceCaptureMode"]; got != config.TraceCaptureMeta {
		t.Fatalf("traceCaptureMode = %v, want %q", got, config.TraceCaptureMeta)
	}
}

func TestSettingsRoundTripsTraceCapture(t *testing.T) {
	h := newTraceSettingsHandler(t)
	postSettings(t, h, `{"traceCaptureMode":"redacted","traceRetentionHours":24,"traceMaxBodyBytes":4096}`)

	body := getSettings(t, h)
	if body["traceCaptureMode"] != config.TraceCaptureRedacted {
		t.Fatalf("traceCaptureMode = %v", body["traceCaptureMode"])
	}
	if body["traceRetentionHours"].(float64) != 24 {
		t.Fatalf("traceRetentionHours = %v", body["traceRetentionHours"])
	}
	if body["traceMaxBodyBytes"].(float64) != 4096 {
		t.Fatalf("traceMaxBodyBytes = %v", body["traceMaxBodyBytes"])
	}
}

// "full" without the acknowledgement must be reported as the "redacted" it
// actually behaves as. Reporting the requested value would mislead an operator
// into believing verbatim prompts are being kept (or, worse, that they are not).
func TestSettingsFullWithoutAcknowledgementReportsRedacted(t *testing.T) {
	h := newTraceSettingsHandler(t)
	postSettings(t, h, `{"traceCaptureMode":"full","traceCaptureAcknowledgeRisk":false}`)

	body := getSettings(t, h)
	if body["traceCaptureMode"] != config.TraceCaptureRedacted {
		t.Fatalf("unacknowledged full must degrade, got %v", body["traceCaptureMode"])
	}
	if body["traceCaptureAcknowledgeRisk"] != false {
		t.Fatalf("acknowledgement should be false, got %v", body["traceCaptureAcknowledgeRisk"])
	}
}

func TestSettingsFullWithAcknowledgementIsHonoured(t *testing.T) {
	h := newTraceSettingsHandler(t)
	postSettings(t, h, `{"traceCaptureMode":"full","traceCaptureAcknowledgeRisk":true}`)

	if got := getSettings(t, h)["traceCaptureMode"]; got != config.TraceCaptureFull {
		t.Fatalf("traceCaptureMode = %v, want full", got)
	}
}

// A partial patch must not silently reset the other trace fields to defaults.
func TestSettingsPartialPatchPreservesOtherTraceFields(t *testing.T) {
	h := newTraceSettingsHandler(t)
	postSettings(t, h, `{"traceCaptureMode":"redacted","traceRetentionHours":48,"traceMaxBodyBytes":8192}`)
	postSettings(t, h, `{"traceRetentionHours":72}`)

	body := getSettings(t, h)
	if body["traceCaptureMode"] != config.TraceCaptureRedacted {
		t.Fatalf("mode lost on partial patch: %v", body["traceCaptureMode"])
	}
	if body["traceMaxBodyBytes"].(float64) != 8192 {
		t.Fatalf("maxBodyBytes lost on partial patch: %v", body["traceMaxBodyBytes"])
	}
	if body["traceRetentionHours"].(float64) != 72 {
		t.Fatalf("traceRetentionHours = %v, want 72", body["traceRetentionHours"])
	}
}

// Changing what is retained about prompts is privacy-relevant, so it must be
// auditable after the fact.
func TestTraceCaptureChangeIsAudited(t *testing.T) {
	h := newTraceSettingsHandler(t)
	postSettings(t, h, `{"traceCaptureMode":"redacted"}`)

	h.auditLogsMu.RLock()
	defer h.auditLogsMu.RUnlock()
	found := false
	for _, entry := range h.auditLogs {
		if entry.Category == "settings" && entry.Action == "trace-capture" {
			found = true
			if entry.SafeDetails["mode"] != config.TraceCaptureRedacted {
				t.Fatalf("audit recorded mode %q", entry.SafeDetails["mode"])
			}
		}
	}
	if !found {
		t.Fatalf("expected a settings/trace-capture audit entry, got %#v", h.auditLogs)
	}
}

func TestApiGetTraceStorageReportsUsage(t *testing.T) {
	h := newTraceSettingsHandler(t)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// Seed one index file and one body file so both tiers are exercised.
	if err := os.MkdirAll(filepath.Join(tmp, tracesDirPath, "bodies", "20260725"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, tracesDirPath, "index-20260725.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, tracesDirPath, "bodies", "20260725", "trc_a.json.gz"), []byte("xxxx"), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	rec := httptest.NewRecorder()
	h.apiGetTraceStorage(rec, httptest.NewRequest(http.MethodGet, "/logs/storage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["indexFiles"].(float64) != 1 {
		t.Fatalf("indexFiles = %v", body["indexFiles"])
	}
	// The body tier lives in day subdirectories, so it must be walked, not
	// counted at the top level only.
	if body["bodyFiles"].(float64) != 1 {
		t.Fatalf("bodyFiles = %v, want 1 (day subdirectory must be walked)", body["bodyFiles"])
	}
	if body["bodyBytes"].(float64) != 4 {
		t.Fatalf("bodyBytes = %v", body["bodyBytes"])
	}
}
