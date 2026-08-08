package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// apiGetLogsFacets serves GET /admin/api/logs/facets: the distinct values present
// in the retained window, so the UI can offer dropdowns instead of making an
// operator guess substrings.
//
// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): fork-only handler. It was lost in
// the union (its route in handleAdminAPI survived, so the merged tree failed to
// build), and is restored here beside the sibling trace handlers it belongs with.
func (h *Handler) apiGetLogsFacets(w http.ResponseWriter, r *http.Request) {
	facets := computeTraceFacets(h.getRequestLogs())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"models":      facets.Models,
		"apis":        facets.APIs,
		"accounts":    facets.Accounts,
		"errorTypes":  facets.ErrorTypes,
		"outcomes":    facets.Outcomes,
		"captureMode": config.GetTraceCaptureMode(),
	})
}

// apiGetTraceStorage reports on-disk trace usage so an operator can see the cost
// of the current capture mode before turning body capture up.
func (h *Handler) apiGetTraceStorage(w http.ResponseWriter, r *http.Request) {
	indexFiles, indexBytes := dirUsage(tracesDir(), false)
	bodyFiles, bodyBytes := dirUsage(traceBodiesDir(), true)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"captureMode":     config.GetTraceCaptureMode(),
		"retentionHours":  config.GetTraceRetentionHours(),
		"maxBodyBytes":    config.GetTraceMaxBodyBytes(),
		"indexFiles":      indexFiles,
		"indexBytes":      indexBytes,
		"bodyFiles":       bodyFiles,
		"bodyBytes":       bodyBytes,
		"droppedRecords":  h.traceStore.Dropped(),
		"writtenRecords":  h.traceStore.Written(),
		"tracesDirectory": tracesDir(),
	})
}

// dirUsage counts files and bytes in a directory. recurse walks day
// subdirectories (the body tier); otherwise only top-level files are counted.
func dirUsage(dir string, recurse bool) (int, int64) {
	var files int
	var bytes int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if !recurse {
				continue
			}
			subFiles, subBytes := dirUsage(filepath.Join(dir, entry.Name()), false)
			files += subFiles
			bytes += subBytes
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files++
		bytes += info.Size()
	}
	return files, bytes
}
func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}
func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
func (h *Handler) apiGetMetricsSummary(w http.ResponseWriter, r *http.Request) {
	logs := h.getRequestLogs()
	byEndpoint := map[string]int{}
	byErrorType := map[string]int{}
	recentErrors := make([]RequestLog, 0, 10)
	var totalDuration int64
	var durationCount int64
	for _, log := range logs {
		byEndpoint[log.Endpoint]++
		if log.Status == "error" {
			byErrorType[log.ErrorType]++
			if len(recentErrors) < 10 {
				recentErrors = append(recentErrors, log)
			}
		}
		if log.Duration > 0 {
			totalDuration += log.Duration
			durationCount++
		}
	}
	avgDuration := int64(0)
	if durationCount > 0 {
		avgDuration = totalDuration / durationCount
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"logCount":        len(logs),
		"byEndpoint":      byEndpoint,
		"byErrorType":     byErrorType,
		"avgDurationMs":   avgDuration,
		"recentErrors":    recentErrors,
		"persistedPath":   requestLogsPath,
	})
}
func filterRequestLogs(logs []RequestLog, status, query string, limit int) []RequestLog {
	status = strings.ToLower(strings.TrimSpace(status))
	query = strings.ToLower(strings.TrimSpace(query))
	if status == "all" {
		status = ""
	}
	filtered := make([]RequestLog, 0, len(logs))
	for _, log := range logs {
		if status != "" && log.Status != status {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(strings.Join([]string{log.Endpoint, log.Model, log.AccountID, log.AccountEmail, log.Status, log.ErrorType, log.Error}, " "))
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		filtered = append(filtered, log)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

// apiGetLogs serves GET /admin/api/logs with structured filters and pagination.
//
// Previously this ran a linear substring scan over the whole ring with the limit
// hardcoded to 0 (unbounded), so the UI always fetched every retained record and
// operators could only narrow by typing substrings. Filters, time windows, a
// clamped limit, and an opaque newest-first cursor now live in trace_query.go.
//
// The legacy logs/count/persistedPath keys are preserved so the existing
// frontend keeps working while it migrates to the paginated shape.
func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := parseTraceQuery(q.Get)
	page := queryRequestLogs(h.getRequestLogs(), filter)

	format := strings.ToLower(q.Get("format"))
	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-logs.csv")
		writeTraceCSV(w, page.Logs)
		return
	case "attempts-csv":
		// One row per upstream attempt, joined by trace ID: this is the export
		// that makes a failover chain analysable in a spreadsheet.
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-attempts.csv")
		writeTraceAttemptsCSV(w, page.Logs)
		return
	case "json":
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-logs.json")
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		// Backward-compatible keys.
		"logs":          page.Logs,
		"count":         len(page.Logs),
		"persistedPath": requestLogsPath,
		// Pagination metadata.
		"total":      page.Total,
		"limit":      filter.Limit,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
		"dropped":    h.traceStore.Dropped(),
	})
}
func (h *Handler) apiGetAuditLogs(w http.ResponseWriter, r *http.Request) {
	h.auditLogsMu.RLock()
	logs := make([]AuditLog, len(h.auditLogs))
	for i, e := range h.auditLogs {
		logs[len(h.auditLogs)-1-i] = e
	}
	h.auditLogsMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "logs": logs, "count": len(logs), "persistedPath": auditLogsPath})
}
func (h *Handler) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	h.requestLogsMu.Lock()
	cleared := len(h.requestLogs)
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	_ = os.Remove(requestLogsPath)
	// Clearing destroys evidence, so the action itself must leave a trail.
	h.auditLogClear(cleared)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// logEntryJSON is the wire shape for a captured log line.
type logEntryJSON struct {
	Ts    int64  `json:"ts"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

func toLogEntryJSON(e logger.Entry) logEntryJSON {
	return logEntryJSON{
		Ts:    e.Time.UnixMilli(),
		Level: logger.LevelName(e.Level),
		Text:  e.Text,
	}
}

// apiStreamLogs GET /admin/api/logs/stream - Server-Sent Events stream of log lines.
func (h *Handler) apiStreamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Streaming not supported"})
		return
	}

	// handleAdminAPI sets application/json; override for SSE.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEntry := func(e logger.Entry) {
		data, _ := json.Marshal(toLogEntryJSON(e))
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	// Backfill retained history first, then stream live entries.
	for _, e := range logger.History() {
		writeEntry(e)
	}
	flusher.Flush()

	ch, cancel := logger.Subscribe()
	defer cancel()

	ctx := r.Context()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			writeEntry(e)
			// Coalesce: drain everything already queued and write it in one
			// batch so a burst costs a single flush (one socket write) instead
			// of one per line.
			for drained := true; drained; {
				select {
				case e2 := <-ch:
					writeEntry(e2)
				default:
					drained = false
				}
			}
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// apiGetLogLevel GET /admin/api/logs/level - returns the active log level.
func (h *Handler) apiGetLogLevel(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"level": logger.LevelName(logger.GetLevel())})
}

// apiSetLogLevel POST /admin/api/logs/level - changes the active log level at runtime and persists it.
func (h *Handler) apiSetLogLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	lvl, ok := logger.ParseLevel(req.Level)
	if !ok {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid level, must be: debug, info, warn, or error"})
		return
	}
	logger.SetLevel(lvl)
	if err := config.UpdateLogLevel(logger.LevelName(lvl)); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"level": logger.LevelName(lvl)})
}
