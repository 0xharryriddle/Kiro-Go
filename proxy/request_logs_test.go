package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestLogsPersistAndReload(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	h := &Handler{}
	h.appendRequestLog(RequestLog{Time: time.Now().Unix(), Endpoint: "claude", Status: "error", ErrorType: "quota", Error: "429"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(tmp, requestLogsPath)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for request log persistence")
		}
		time.Sleep(20 * time.Millisecond)
	}

	reloaded := &Handler{}
	reloaded.loadRequestLogs()
	logs := reloaded.getRequestLogs()
	if len(logs) != 1 || logs[0].ErrorType != "quota" {
		t.Fatalf("unexpected reloaded logs: %#v", logs)
	}
}

func TestApiGetLogsFiltersAndExportsCSV(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{
		{Time: 1, Endpoint: "openai", Model: "gpt", AccountID: "acc-ok", AccountEmail: "ok@example.com", Status: "success", Tokens: 4},
		{Time: 2, Endpoint: "claude", Model: "sonnet", AccountID: "acc-bad", AccountEmail: "bad@example.com", Status: "error", ErrorType: "quota", Error: "quota exceeded"},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/logs?status=error&q=quota&format=csv", nil)
	h.apiGetLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "quota exceeded") || !strings.Contains(body, "bad@example.com") || strings.Contains(body, "acc-ok") {
		t.Fatalf("unexpected csv export: %s", body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/csv") {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestMetricsSummaryAggregatesLogs(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{
		{Time: 3, Endpoint: "claude", Status: "error", ErrorType: "quota"},
		{Time: 2, Endpoint: "openai", Status: "success", Duration: 100},
		{Time: 1, Endpoint: "openai", Status: "success", Duration: 300},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics/summary", nil)
	h.apiGetMetricsSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["avgDurationMs"].(float64) != 200 {
		t.Fatalf("avgDurationMs = %v", body["avgDurationMs"])
	}
	encoded := rec.Body.String()
	if !strings.Contains(encoded, `"quota":1`) || !strings.Contains(encoded, `"openai":2`) {
		t.Fatalf("summary missing aggregates: %s", encoded)
	}
}
