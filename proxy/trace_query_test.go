package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedQueryLogs builds a deterministic corpus spanning models, accounts,
// outcomes and durations. Times ascend with index so newest-first ordering is
// unambiguous.
func seedQueryLogs(n int) []RequestLog {
	logs := make([]RequestLog, 0, n)
	for i := 0; i < n; i++ {
		entry := RequestLog{
			Time:       int64(1000 + i),
			RequestID:  fmt.Sprintf("trc_%03d", i),
			Endpoint:   "claude",
			API:        "claude",
			Model:      "sonnet",
			AccountID:  "acc-a",
			Status:     "success",
			Outcome:    outcomeSuccess,
			HTTPStatus: 200,
			Duration:   int64(10 * i),
			ApiKeyID:   "key-1",
		}
		if i%2 == 1 {
			entry.API = "openai"
			entry.Endpoint = "openai"
			entry.Model = "gpt"
			entry.AccountID = "acc-b"
			entry.ApiKeyID = "key-2"
		}
		if i%5 == 0 {
			entry.Status = "error"
			entry.Outcome = outcomeError
			entry.ErrorType = "quota"
			entry.HTTPStatus = 429
			entry.Error = "quota exhausted"
		}
		logs = append(logs, entry)
	}
	return logs
}

func decodeLogsResponse(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Logs       []RequestLog `json:"logs"`
	Count      int          `json:"count"`
	NextCursor string       `json:"nextCursor"`
	HasMore    bool         `json:"hasMore"`
	Total      int          `json:"total"`
} {
	t.Helper()
	var body struct {
		Logs       []RequestLog `json:"logs"`
		Count      int          `json:"count"`
		NextCursor string       `json:"nextCursor"`
		HasMore    bool         `json:"hasMore"`
		Total      int          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return body
}

// The previous implementation hardcoded limit=0 (unbounded), so the UI could be
// handed the entire retained corpus in one response.
func TestApiGetLogsHonoursLimitAndClamps(t *testing.T) {
	h := &Handler{}
	h.requestLogs = seedQueryLogs(50)

	rec := httptest.NewRecorder()
	h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs?limit=10", nil))
	body := decodeLogsResponse(t, rec)
	if len(body.Logs) != 10 {
		t.Fatalf("limit=10 returned %d rows", len(body.Logs))
	}
	if !body.HasMore || body.NextCursor == "" {
		t.Fatal("expected hasMore + nextCursor when more rows remain")
	}

	// Absurd limits must clamp rather than being honoured or rejected.
	rec = httptest.NewRecorder()
	h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs?limit=99999", nil))
	if got := len(decodeLogsResponse(t, rec).Logs); got != 50 {
		t.Fatalf("oversize limit returned %d rows, want all 50", got)
	}

	// A negative limit falls back to the default rather than returning nothing.
	rec = httptest.NewRecorder()
	h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs?limit=-5", nil))
	if got := len(decodeLogsResponse(t, rec).Logs); got == 0 {
		t.Fatal("negative limit must not yield an empty page")
	}
}

// Paging must cover the corpus exactly once: no duplicates, no gaps.
func TestApiGetLogsCursorPaginationIsStable(t *testing.T) {
	h := &Handler{}
	h.requestLogs = seedQueryLogs(25)

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		url := "/logs?limit=7"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := httptest.NewRecorder()
		h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, url, nil))
		body := decodeLogsResponse(t, rec)
		for _, l := range body.Logs {
			if seen[l.RequestID] {
				t.Fatalf("duplicate row across pages: %s", l.RequestID)
			}
			seen[l.RequestID] = true
		}
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		if !body.HasMore {
			break
		}
		cursor = body.NextCursor
	}
	if len(seen) != 25 {
		t.Fatalf("pagination covered %d of 25 rows", len(seen))
	}
}

func TestApiGetLogsStructuredFilters(t *testing.T) {
	h := &Handler{}
	h.requestLogs = seedQueryLogs(30)

	cases := []struct {
		name  string
		query string
		check func(t *testing.T, logs []RequestLog)
	}{
		{"api", "api=openai", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.API != "openai" {
					t.Fatalf("api filter leaked %q", l.API)
				}
			}
		}},
		{"model", "model=gpt", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.Model != "gpt" {
					t.Fatalf("model filter leaked %q", l.Model)
				}
			}
		}},
		{"accountId", "accountId=acc-a", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.AccountID != "acc-a" {
					t.Fatalf("accountId filter leaked %q", l.AccountID)
				}
			}
		}},
		{"apiKeyId", "apiKeyId=key-2", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.ApiKeyID != "key-2" {
					t.Fatalf("apiKeyId filter leaked %q", l.ApiKeyID)
				}
			}
		}},
		{"errorType", "errorType=quota", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.ErrorType != "quota" {
					t.Fatalf("errorType filter leaked %q", l.ErrorType)
				}
			}
		}},
		{"outcome", "outcome=" + outcomeError, func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.Outcome != outcomeError {
					t.Fatalf("outcome filter leaked %q", l.Outcome)
				}
			}
		}},
		{"minDuration", "minDurationMs=200", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.Duration < 200 {
					t.Fatalf("minDurationMs leaked %dms", l.Duration)
				}
			}
		}},
		{"timeRange", "from=1005&to=1010", func(t *testing.T, logs []RequestLog) {
			for _, l := range logs {
				if l.Time < 1005 || l.Time > 1010 {
					t.Fatalf("time filter leaked t=%d", l.Time)
				}
			}
			if len(logs) == 0 {
				t.Fatal("expected rows inside the window")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs?limit=1000&"+tc.query, nil))
			body := decodeLogsResponse(t, rec)
			if len(body.Logs) == 0 {
				t.Fatalf("filter %q matched nothing", tc.query)
			}
			tc.check(t, body.Logs)
		})
	}
}

// The old response keys must survive so the frontend keeps working during the
// migration.
func TestApiGetLogsKeepsBackwardCompatibleKeys(t *testing.T) {
	h := &Handler{}
	h.requestLogs = seedQueryLogs(3)

	rec := httptest.NewRecorder()
	h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	raw := rec.Body.String()
	for _, key := range []string{`"logs"`, `"count"`, `"persistedPath"`} {
		if !strings.Contains(raw, key) {
			t.Fatalf("response dropped backward-compatible key %s: %s", key, raw)
		}
	}
}

func TestApiGetLogsFacets(t *testing.T) {
	h := &Handler{}
	h.requestLogs = seedQueryLogs(20)

	rec := httptest.NewRecorder()
	h.apiGetLogsFacets(rec, httptest.NewRequest(http.MethodGet, "/logs/facets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Success   bool           `json:"success"`
		Models    []string       `json:"models"`
		APIs      []string       `json:"apis"`
		Accounts  []string       `json:"accounts"`
		ErrorType []string       `json:"errorTypes"`
		Outcomes  map[string]int `json:"outcomes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Models) != 2 {
		t.Fatalf("expected 2 distinct models, got %v", body.Models)
	}
	if len(body.APIs) != 2 {
		t.Fatalf("expected 2 distinct apis, got %v", body.APIs)
	}
	if body.Outcomes[outcomeError] == 0 {
		t.Fatalf("expected error outcomes to be counted: %v", body.Outcomes)
	}
	// Facets must be sorted so the UI dropdown order is stable across polls.
	for i := 1; i < len(body.Models); i++ {
		if body.Models[i-1] > body.Models[i] {
			t.Fatalf("models not sorted: %v", body.Models)
		}
	}
}

// CSV export must carry the new trace columns, not just the original eleven.
func TestApiGetLogsCSVIncludesTraceColumns(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{{
		Time: 1, RequestID: "trc_x", Endpoint: "claude", API: "claude", Model: "sonnet",
		Status: "success", Outcome: outcomeSuccess, HTTPStatus: 200,
		InputTokens: 5, OutputTokens: 7, Tokens: 12, TTFBMs: 42, AttemptCount: 2,
		Region: "eu-central-1",
	}}

	rec := httptest.NewRecorder()
	h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs?format=csv", nil))
	body := rec.Body.String()
	for _, col := range []string{"traceId", "outcome", "httpStatus", "inputTokens", "outputTokens", "ttfbMs", "attemptCount", "region"} {
		if !strings.Contains(body, col) {
			t.Fatalf("csv header missing %q: %s", col, body)
		}
	}
	if !strings.Contains(body, "trc_x") || !strings.Contains(body, "eu-central-1") {
		t.Fatalf("csv rows missing trace values: %s", body)
	}
}

// One row per attempt, joinable by trace ID: this is the view that explains a
// failover chain in a spreadsheet.
func TestApiGetLogsAttemptsCSV(t *testing.T) {
	h := &Handler{}
	h.requestLogs = []RequestLog{{
		Time: 1, RequestID: "trc_y", API: "claude", Model: "sonnet",
		Status: "success", Outcome: outcomeSuccess,
		Attempts: []TraceAttempt{
			{Seq: 1, AccountID: "acc-1", Region: "us-east-1", Outcome: outcomeError, ErrorType: "quota", HTTPStatus: 429},
			{Seq: 2, AccountID: "acc-2", Region: "eu-central-1", Outcome: outcomeSuccess, HTTPStatus: 200},
		},
		AttemptCount: 2,
	}}

	rec := httptest.NewRecorder()
	h.apiGetLogs(rec, httptest.NewRequest(http.MethodGet, "/logs?format=attempts-csv", nil))
	body := rec.Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 attempt rows, got %d: %s", len(lines), body)
	}
	if !strings.Contains(lines[0], "seq") || !strings.Contains(lines[0], "traceId") {
		t.Fatalf("attempts csv header wrong: %s", lines[0])
	}
	if !strings.Contains(body, "acc-1") || !strings.Contains(body, "acc-2") {
		t.Fatalf("attempts csv lost per-attempt accounts: %s", body)
	}
}
