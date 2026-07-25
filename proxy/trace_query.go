package proxy

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// traceQueryDefaultLimit bounds a page when the caller does not ask.
	// Previously the limit was hardcoded to 0 (unbounded), so the admin UI could
	// be handed the whole retained corpus in a single response.
	traceQueryDefaultLimit = 100
	// traceQueryMaxLimit caps what a caller may request.
	traceQueryMaxLimit = 1000
)

// traceQuery is a parsed set of log filters.
//
// Every field is optional; a zero value matches everything, which keeps the
// endpoint backward compatible with callers that pass no parameters at all.
type traceQuery struct {
	Status        string
	Outcome       string
	API           string
	Model         string
	AccountID     string
	ApiKeyID      string
	ErrorType     string
	Text          string
	From          int64
	To            int64
	MinDurationMs int64
	Limit         int
	Cursor        string
}

// parseTraceQuery reads filters from URL query values. Unparseable numbers are
// ignored rather than rejected: a malformed filter should not fail an operator's
// whole request, it should simply not narrow the result.
func parseTraceQuery(get func(string) string) traceQuery {
	q := traceQuery{
		Status:    normalizeFilter(get("status")),
		Outcome:   normalizeFilter(get("outcome")),
		API:       normalizeFilter(get("api")),
		Model:     normalizeFilter(get("model")),
		AccountID: strings.TrimSpace(get("accountId")),
		ApiKeyID:  strings.TrimSpace(get("apiKeyId")),
		ErrorType: normalizeFilter(get("errorType")),
		Text:      strings.ToLower(strings.TrimSpace(get("q"))),
		Cursor:    strings.TrimSpace(get("cursor")),
	}
	q.From = parseInt64OrZero(get("from"))
	q.To = parseInt64OrZero(get("to"))
	q.MinDurationMs = parseInt64OrZero(get("minDurationMs"))
	q.Limit = clampTraceLimit(get("limit"))
	return q
}

// normalizeFilter lowercases a filter and treats the sentinel "all" as unset,
// matching the existing status-filter contract used by the admin UI.
func normalizeFilter(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "all" {
		return ""
	}
	return v
}

func parseInt64OrZero(raw string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// clampTraceLimit resolves the page size. An absent, zero, negative, or
// unparseable value yields the default; anything above the ceiling is clamped
// rather than rejected, so a client cannot accidentally request the whole store.
func clampTraceLimit(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return traceQueryDefaultLimit
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return traceQueryDefaultLimit
	}
	if v > traceQueryMaxLimit {
		return traceQueryMaxLimit
	}
	return v
}

// matchesTraceQuery reports whether one record satisfies every active filter.
func matchesTraceQuery(log RequestLog, q traceQuery) bool {
	if q.Status != "" && strings.ToLower(log.Status) != q.Status {
		return false
	}
	if q.Outcome != "" && strings.ToLower(log.Outcome) != q.Outcome {
		return false
	}
	if q.API != "" && !equalFoldEither(q.API, log.API, log.Endpoint) {
		return false
	}
	if q.Model != "" && !strings.EqualFold(log.Model, q.Model) {
		return false
	}
	if q.AccountID != "" && log.AccountID != q.AccountID {
		return false
	}
	if q.ApiKeyID != "" && log.ApiKeyID != q.ApiKeyID {
		return false
	}
	if q.ErrorType != "" && !strings.EqualFold(log.ErrorType, q.ErrorType) {
		return false
	}
	if q.From > 0 && log.Time < q.From {
		return false
	}
	if q.To > 0 && log.Time > q.To {
		return false
	}
	if q.MinDurationMs > 0 && log.Duration < q.MinDurationMs {
		return false
	}
	if q.Text != "" && !traceTextMatches(log, q.Text) {
		return false
	}
	return true
}

// equalFoldEither lets the api filter match either the new API field or the
// legacy Endpoint field, so records written before Phase 1 remain queryable.
func equalFoldEither(want, a, b string) bool {
	return strings.EqualFold(a, want) || strings.EqualFold(b, want)
}

// traceTextMatches performs the free-text search. The trace ID and per-attempt
// error text are included, because "find the request a client complained about"
// and "find every request that hit a quota error" are the two most common
// searches and both were previously impossible.
func traceTextMatches(log RequestLog, needle string) bool {
	fields := []string{
		log.Endpoint, log.API, log.Model, log.ResponseModel, log.AccountID, log.AccountEmail,
		log.Status, log.Outcome, log.ErrorType, log.Error, log.RequestID, log.ApiKeyID,
		log.Region, log.UpstreamHost, log.StopReason,
	}
	for _, f := range fields {
		if f != "" && strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	for _, a := range log.Attempts {
		for _, f := range []string{a.AccountID, a.AccountEmail, a.ErrorType, a.Error, a.Region, a.UpstreamHost, a.UpstreamRequestID} {
			if f != "" && strings.Contains(strings.ToLower(f), needle) {
				return true
			}
		}
	}
	return false
}

// encodeTraceCursor builds an opaque cursor from a record's position.
//
// It embeds both the timestamp and the trace ID: timestamps have one-second
// resolution and collide freely under load, so a time-only cursor would skip or
// repeat rows within the same second.
func encodeTraceCursor(log RequestLog) string {
	raw := fmt.Sprintf("%d|%s", log.Time, log.RequestID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeTraceCursor parses a cursor. An unreadable cursor yields ok=false and
// the caller starts from the beginning rather than erroring, so a stale
// bookmark degrades to "first page" instead of a broken UI.
func decodeTraceCursor(cursor string) (int64, string, bool) {
	if strings.TrimSpace(cursor) == "" {
		return 0, "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", false
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return ts, parts[1], true
}

// traceQueryResult is one page of filtered records.
type traceQueryResult struct {
	Logs       []RequestLog
	Total      int
	NextCursor string
	HasMore    bool
}

// queryRequestLogs applies filters, then cursor pagination, over a newest-first
// input slice.
func queryRequestLogs(logs []RequestLog, q traceQuery) traceQueryResult {
	matched := make([]RequestLog, 0, len(logs))
	for _, log := range logs {
		if matchesTraceQuery(log, q) {
			matched = append(matched, log)
		}
	}

	start := 0
	if ts, id, ok := decodeTraceCursor(q.Cursor); ok {
		// Resume immediately after the cursor row. Falling back to 0 when the
		// row is gone (pruned or rotated away mid-page) is deliberate: showing
		// the first page again is recoverable, silently returning nothing is not.
		for i, log := range matched {
			if log.Time == ts && log.RequestID == id {
				start = i + 1
				break
			}
		}
	}
	if start > len(matched) {
		start = len(matched)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = traceQueryDefaultLimit
	}
	end := start + limit
	if end > len(matched) {
		end = len(matched)
	}

	page := matched[start:end]
	result := traceQueryResult{
		Logs:    page,
		Total:   len(matched),
		HasMore: end < len(matched),
	}
	if result.HasMore && len(page) > 0 {
		result.NextCursor = encodeTraceCursor(page[len(page)-1])
	}
	return result
}

// traceFacets summarises the distinct values available for filtering.
type traceFacets struct {
	Models     []string       `json:"models"`
	APIs       []string       `json:"apis"`
	Accounts   []string       `json:"accounts"`
	ErrorTypes []string       `json:"errorTypes"`
	Outcomes   map[string]int `json:"outcomes"`
}

// computeTraceFacets derives filter options from the retained records, so the
// UI can offer dropdowns instead of making an operator guess substrings.
func computeTraceFacets(logs []RequestLog) traceFacets {
	models := map[string]bool{}
	apis := map[string]bool{}
	accounts := map[string]bool{}
	errorTypes := map[string]bool{}
	outcomes := map[string]int{}

	for _, log := range logs {
		if log.Model != "" {
			models[log.Model] = true
		}
		api := log.API
		if api == "" {
			api = log.Endpoint
		}
		if api != "" {
			apis[api] = true
		}
		if log.AccountID != "" {
			accounts[log.AccountID] = true
		}
		if log.ErrorType != "" {
			errorTypes[log.ErrorType] = true
		}
		outcome := log.Outcome
		if outcome == "" {
			// Pre-Phase-1 records carry only Status; map it so counts stay honest.
			outcome = log.Status
		}
		if outcome != "" {
			outcomes[outcome]++
		}
	}

	return traceFacets{
		Models:     sortedKeys(models),
		APIs:       sortedKeys(apis),
		Accounts:   sortedKeys(accounts),
		ErrorTypes: sortedKeys(errorTypes),
		Outcomes:   outcomes,
	}
}

// sortedKeys returns map keys in a stable order so UI dropdowns do not reshuffle
// between polls.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
