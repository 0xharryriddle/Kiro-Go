package proxy

import (
	"encoding/json"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApiReplayDiagnoseValidOpenAIDryRun(t *testing.T) {
	h := &Handler{pool: &pool.AccountPool{}}
	body := `{"endpoint":"openai","payload":{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}],"max_tokens":64}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/replay/diagnose", strings.NewReader(body))
	h.apiReplayDiagnose(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["status"] != "valid" || got["dryRun"] != true || got["translated"] != true {
		t.Fatalf("unexpected response: %#v", got)
	}
	if got["estimatedTokens"].(float64) <= 0 {
		t.Fatalf("expected positive token estimate, got %#v", got)
	}
}

func TestApiReplayDiagnoseInvalidOpenAIShape(t *testing.T) {
	h := &Handler{pool: &pool.AccountPool{}}
	body := `{"endpoint":"openai","payload":{"model":"claude-sonnet-4.5","messages":[]}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/replay/diagnose", strings.NewReader(body))
	h.apiReplayDiagnose(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for dry-run validation result, got %d", rec.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["status"] != "invalid" || got["validationError"] == "" {
		t.Fatalf("unexpected response: %#v", got)
	}
}
