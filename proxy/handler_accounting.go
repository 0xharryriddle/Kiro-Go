package proxy

import (
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"strings"
	"sync/atomic"
	"time"
)

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
// Hot-path counter updates (global stats, per-key usage, per-account stats) only
// mark config dirty; this coalesces them into one disk write per tick instead of
// writing the whole config under cfgLock on every request completion.
func (h *Handler) saveStats() {
	config.UpdateStats(
		atomic.LoadInt64(&h.totalRequests),
		atomic.LoadInt64(&h.successRequests),
		atomic.LoadInt64(&h.failedRequests),
		atomic.LoadInt64(&h.totalTokens),
		h.getCredits(),
	)
	if err := config.FlushDirty(); err != nil {
		logger.Warnf("[StatsSaver] Failed to persist config: %v", err)
	}
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordFailure counts a terminal request failure against the global counters.
//
// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): this is upstream's helper and it is
// kept, because upstream's recordFailureForApiKey (the failure path every Claude /
// OpenAI / Responses route now calls) depends on it. The fork counts the same
// failure inside emitTrace instead; the two must not both run for one request, so
// emitTrace stays the single terminal record on trace-wired routes and this
// helper serves the per-key failure path.
func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
// model is recorded in the per-request log for the admin API Log view.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64, model string, account *config.Account, endpoint string, startedAt time.Time) {
	h.recordSuccess(inputTokens, outputTokens, credits)

	keyName, keyMasked := apiKeyLabels(apiKeyID)
	if apiKeyID != "" {
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fork's
		// RecordApiKeyUsage carries `model` (per-model usage accounting) and folds
		// the real token count back into the F6 rate window; upstream adds the
		// usageStats collaborator. Both are kept — dropping the rateLimiter
		// feedback would leave every TPM check reading only ESTIMATED tokens.
		if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits, model); err != nil {
			logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
		}
		// F6: fold actual tokens into the key's rate window (best-effort TPM), so the
		// next request's TPM check reflects real consumption.
		if h.rateLimiter != nil {
			h.rateLimiter.RecordTokens(apiKeyID, int64(inputTokens+outputTokens), time.Now().Unix())
		}
		if h.usage != nil {
			h.usage.recordSuccess(apiKeyID, model, int64(inputTokens), 0, int64(outputTokens))
		}
	}

	accountID := ""
	accountEmail := ""
	if account != nil {
		accountID = account.ID
		accountEmail = account.Email
	}

	logRequest(RequestLogEntry{
		Status:       "ok",
		Endpoint:     endpoint,
		APIKeyID:     apiKeyID,
		APIKeyName:   keyName,
		APIKeyMasked: keyMasked,
		Model:        model,
		AccountID:    accountID,
		AccountEmail: accountEmail,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Credits:      credits,
		DurationMs:   durationMs(startedAt),
	})
}

// apiKeyLabels resolves the display name + masked value for a key id (both empty when unknown).
func apiKeyLabels(apiKeyID string) (name, masked string) {
	if apiKeyID == "" {
		return "", ""
	}
	if entry := config.GetApiKeyEntry(apiKeyID); entry != nil {
		return entry.Name, config.MaskApiKey(entry.Key)
	}
	return "", ""
}
func durationMs(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	return time.Since(startedAt).Milliseconds()
}

// recordFailureForApiKey is recordFailure + a failure entry in the per-request log so the
// admin API Log view shows what went wrong (endpoint, model, status code, error detail).
func (h *Handler) recordFailureForApiKey(apiKeyID, endpoint, model string, statusCode int, errMsg string, startedAt time.Time) {
	h.recordFailure()
	h.recordFailureAttribution(apiKeyID, endpoint, model, statusCode, errMsg, startedAt)
}

// recordFailureAttribution is recordFailureForApiKey WITHOUT the global counter
// bump: per-key usage attribution and the flat request-log entry only.
//
// It exists because emitTrace already owns failure counting on traced routes —
// `if outcome == outcomeError { totalRequests++; failedRequests++ }`
// (request_trace_recorder.go:362-365) — so a route that called BOTH emitTrace and
// recordFailureForApiKey counted one failed request TWICE in both counters. That
// is precisely what the note above this block warns against ("that route would
// then log twice and double-count totalRequests"); `/v1/responses` streaming was
// doing it at responses_handler.go:752-753.
//
// Use this variant wherever emitTrace(outcomeError) is also called, and the
// counting variant everywhere else, so a failed request is counted exactly once
// on every path.
func (h *Handler) recordFailureAttribution(apiKeyID, endpoint, model string, statusCode int, errMsg string, startedAt time.Time) {
	if apiKeyID != "" && h.usage != nil {
		h.usage.recordFailure(apiKeyID, model)
	}
	name, masked := apiKeyLabels(apiKeyID)
	logRequest(RequestLogEntry{
		Status:       "error",
		Endpoint:     endpoint,
		APIKeyID:     apiKeyID,
		APIKeyName:   name,
		APIKeyMasked: masked,
		Model:        model,
		StatusCode:   statusCode,
		Error:        errMsg,
		DurationMs:   durationMs(startedAt),
	})
}

// The Claude / OpenAI / Responses routes log through traceRecorder +
// Handler.emitTrace (proxy/request_trace_recorder.go), which emits exactly ONE
// terminal record per client request with every failover attempt embedded.
//
// recordSuccessLog / recordFailureWithDetails below are the flat single-record
// helpers, and they must NOT be deleted.
//
// UPDATED (round 18e / PROPOSAL D1). This note used to name custom_api and native
// Bedrock as "the subsystems that have no trace-recorder wiring". Both are wired
// now: their success recorders go through recordPassthroughTrace
// (proxy/passthrough_trace.go), which emits the rich trace row when the dispatch
// loop threaded a recorder through forwardParams.
//
// The flat helpers survive for two reasons:
//   - recordPassthroughTrace falls back to recordSuccessLog when no recorder was
//     threaded (bedrockTestReply, admin probes, and the many test literals that
//     build forwardParams by hand);
//   - the websearch pair still calls recordSuccessLog directly
//     (websearch.go, websearch_loop.go). Those are not forwardParams
//     passthroughs — websearch_loop bills SEVERAL accounts per request — so they
//     need their own design. Tracked as D1b in
//     docs/plans/PROPOSAL_comprehensive_upgrade.md.
//
// The warning still stands, and it is not hypothetical: do NOT pair a counting
// failure helper with emitTrace on the same path. /v1/responses streaming did
// exactly that (emitTrace(outcomeError) + recordFailureForApiKey) and counted one
// failed request twice in totalRequests AND failedRequests. Use
// recordFailureAttribution — the non-counting variant — wherever emitTrace already
// runs.

func requestLogAccountEmail(accountID string) string {
	if strings.TrimSpace(accountID) == "" {
		return ""
	}
	for _, acc := range config.GetAccounts() {
		if acc.ID == accountID {
			return strings.TrimSpace(acc.Email)
		}
	}
	return ""
}

// recordFailureWithDetails records a failure and stores it in the request logs.
// apiKeyID attributes the failed request to the API key entry that issued it so
// customer-facing log endpoints can show per-key failures; empty on legacy paths.
// Do NOT pair this with emitTrace on the same path: emitTrace already appends a
// row and counts failures, so pairing them writes two rows and counts one failure
// twice. A traced path wants emitTrace ALONE (see recordPassthroughPartialFailure).
func (h *Handler) recordFailureWithDetails(endpoint, model, accountID, apiKeyID string, err error) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)

	errMsg := err.Error()
	errType := classifyError(errMsg)

	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ApiKeyID:  apiKeyID,
		Status:    "error",
		Error:     errMsg,
		ErrorType: errType,
	}

	h.appendRequestLog(entry)
}

// recordSuccessLog records a successful request in the request logs.
// apiKeyID attributes the request to the API key entry that issued it (see above).
func (h *Handler) recordSuccessLog(endpoint, model, accountID, apiKeyID string, tokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ApiKeyID:  apiKeyID,
		Status:    "success",
		Tokens:    tokens,
		Credits:   credits,
		Duration:  durationMs,
	}

	h.appendRequestLog(entry)
}
func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	needLegacySnapshot := h.traceStore == nil
	var snapshot []RequestLog
	if needLegacySnapshot {
		snapshot = append([]RequestLog(nil), h.requestLogs...)
	}
	h.requestLogsMu.Unlock()

	if h.traceStore != nil {
		// Append-only JSONL: O(1) per request, single writer, no shared temp
		// path. Durable history lives on disk; the ring above only backs the
		// live view.
		h.traceStore.Append(entry)
		return
	}
	// Legacy whole-file rewrite, retained only for handlers constructed without
	// a store (zero-value Handler in unit tests).
	go persistRequestLogs(snapshot)
}

// classifyError categorizes an error message into a type for display.
func classifyError(msg string) string {
	switch {
	case isQuotaErrorMessage(msg):
		return "quota"
	case isOverageErrorMessage(msg):
		return "overage"
	case isSuspensionErrorMessage(msg):
		return "suspended"
	case pool.IsAuthFailure(errors.New(msg)):
		return "auth"
	case isProfileUnavailableErrorMessage(msg):
		return "profile"
	default:
		return "unknown"
	}
}

// getRequestLogs returns a copy of request logs (newest first).
func (h *Handler) getRequestLogs() []RequestLog {
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	if len(h.requestLogs) == 0 {
		return []RequestLog{}
	}
	result := make([]RequestLog, len(h.requestLogs))
	for i, e := range h.requestLogs {
		result[len(h.requestLogs)-1-i] = e
	}
	return result
}
