package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The detail endpoint must return the full trace record, including per-attempt
// routing detail, so an operator can reconstruct a failover without reading
// files on the server.
func TestApiGetTraceDetailReturnsRecordWithAttempts(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{{
		Time:      10,
		RequestID: "trc_target",
		Endpoint:  "claude",
		API:       "claude",
		Status:    "success",
		Outcome:   outcomeSuccess,
		Attempts: []TraceAttempt{
			{Seq: 1, AccountID: "acc-1", Region: "us-east-1", Outcome: outcomeError, ErrorType: "quota"},
			{Seq: 2, AccountID: "acc-2", Region: "eu-central-1", Outcome: outcomeSuccess, HTTPStatus: 200},
		},
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/logs/trc_target", nil)
	h.apiGetTraceDetail(rec, req, "trc_target")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Trace detail can carry prompt text once body capture is on: it must never
	// be cached by a browser or intermediary.
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var body struct {
		Success bool       `json:"success"`
		Trace   RequestLog `json:"record"`
		Request string     `json:"request"`
		Reply   string     `json:"response"`
		Mode    string     `json:"captureMode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Success {
		t.Fatal("expected success=true")
	}
	if body.Trace.RequestID != "trc_target" {
		t.Fatalf("RequestID = %q", body.Trace.RequestID)
	}
	if len(body.Trace.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(body.Trace.Attempts))
	}
	if body.Trace.Attempts[0].ErrorType != "quota" {
		t.Fatalf("attempt detail lost: %#v", body.Trace.Attempts[0])
	}
	// No bodies were captured, so the response must say so explicitly rather
	// than returning empty strings that look like a bug.
	if body.Mode == "" {
		t.Fatal("captureMode must be reported so absent bodies are explainable")
	}
}

func TestApiGetTraceDetailUnknownIDReturns404(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{{Time: 1, RequestID: "trc_other"}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/logs/trc_missing", nil)
	h.apiGetTraceDetail(rec, req, "trc_missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A trace detail response must never leak an absolute server path: bodyRef is
// relative by contract.
func TestApiGetTraceDetailNeverLeaksAbsolutePaths(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{{
		Time:      5,
		RequestID: "trc_p",
		BodyRef:   "20260725/trc_p.json.gz",
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/logs/trc_p", nil)
	h.apiGetTraceDetail(rec, req, "trc_p")

	raw := rec.Body.String()
	if strings.Contains(raw, "/home/") || strings.Contains(raw, "data/traces/bodies") {
		t.Fatalf("response leaked a filesystem path: %s", raw)
	}
}
