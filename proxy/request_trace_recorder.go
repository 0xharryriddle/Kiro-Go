package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro-go/config"
)

// traceRecorder accumulates one client request's trace as it progresses through
// account selection, upstream dispatch, and failover.
//
// Rationale: before this existed, a failover chain emitted at most one
// RequestLog carrying only the FINAL error, so an attempt that hit a quota error
// and rerouted left no evidence at all — the very evidence needed to explain why
// traffic moved. The recorder collects one TraceAttempt per dispatch and emits
// exactly ONE RequestLog per client request, so attempt detail becomes visible
// without inflating request counts.
//
// Streaming callbacks fire from the serving goroutine at arbitrary points, so
// every mutation is mutex-guarded and the type is safe under -race.
type traceRecorder struct {
	mu sync.Mutex

	traceID   string
	api       string
	model     string
	stream    bool
	apiKeyID  string
	startedAt time.Time

	attempts []TraceAttempt

	ttfbMs       int64
	ttfbRecorded bool

	stopReason    string
	responseModel string
	toolCalls     int

	inputTokens     int
	outputTokens    int
	cacheReadTokens int
	credits         float64
	cacheHit        bool
}

// traceAttempt is a handle for one in-flight dispatch attempt. It is owned by
// the caller between beginAttempt and endAttempt, so nothing is appended to the
// recorder until the attempt actually concludes.
type traceAttempt struct {
	start   time.Time
	attempt TraceAttempt
}

// newTraceRecorder starts a trace for one client request. api is the
// client-facing surface (claude, openai, responses).
func newTraceRecorder(api, model string, stream bool, apiKeyID string) *traceRecorder {
	return &traceRecorder{
		traceID:   newTraceID(),
		api:       api,
		model:     model,
		stream:    stream,
		apiKeyID:  apiKeyID,
		startedAt: time.Now(),
	}
}

// TraceID returns the identifier shared by every record emitted for this request.
func (tr *traceRecorder) TraceID() string {
	if tr == nil {
		return ""
	}
	return tr.traceID
}

// beginAttempt opens a dispatch attempt against account. Safe on a nil recorder
// so instrumentation can be added to paths that do not always trace.
func (tr *traceRecorder) beginAttempt(account *config.Account) *traceAttempt {
	if tr == nil {
		return nil
	}
	tr.mu.Lock()
	seq := len(tr.attempts) + 1
	tr.mu.Unlock()

	now := time.Now()
	h := &traceAttempt{
		start: now,
		attempt: TraceAttempt{
			Seq:         seq,
			StartedAtMs: now.UnixMilli(),
		},
	}
	if account != nil {
		h.attempt.AccountID = account.ID
		h.attempt.AccountEmail = strings.TrimSpace(account.Email)
		h.attempt.Region = effectiveTraceRegion(account)
		h.attempt.ProfileArn = profileArnSuffix(account.ProfileArn)
	}
	return h
}

// applyDiagnostics folds per-endpoint upstream detail (HTTP status, upstream
// request ID, resolved host) collected by CallKiroAPIWithDiagnostics into the
// open attempt.
func (tr *traceRecorder) applyDiagnostics(h *traceAttempt, d *KiroCallDiagnostics) {
	if tr == nil || h == nil || d == nil {
		return
	}
	last := d.Last()
	if last == nil {
		return
	}
	if last.HTTPStatus != 0 {
		h.attempt.HTTPStatus = last.HTTPStatus
	}
	if last.UpstreamRequestID != "" {
		h.attempt.UpstreamRequestID = last.UpstreamRequestID
	}
	if last.UpstreamHost != "" {
		h.attempt.UpstreamHost = last.UpstreamHost
	}
	if last.UpstreamEndpoint != "" {
		h.attempt.UpstreamEndpoint = last.UpstreamEndpoint
	}
	if last.RetryAfter != "" {
		h.attempt.RetryAfter = last.RetryAfter
	}
}

// endAttempt closes an attempt. A nil err records success; a non-nil err records
// the classified failure so a reroute keeps its cause.
func (tr *traceRecorder) endAttempt(h *traceAttempt, err error) {
	if tr == nil || h == nil {
		return
	}
	h.attempt.DurationMs = time.Since(h.start).Milliseconds()
	if err != nil {
		msg := err.Error()
		h.attempt.Outcome = outcomeError
		h.attempt.ErrorType = classifyError(msg)
		h.attempt.Error = scrubTraceText(msg)
	} else {
		h.attempt.Outcome = outcomeSuccess
	}
	tr.mu.Lock()
	tr.attempts = append(tr.attempts, h.attempt)
	tr.mu.Unlock()
}

// markFirstByte records time-to-first-token the first time output is produced.
// Subsequent calls are ignored, so it is safe to call from a hot streaming
// callback.
func (tr *traceRecorder) markFirstByte() {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.ttfbRecorded {
		return
	}
	tr.ttfbRecorded = true
	tr.ttfbMs = time.Since(tr.startedAt).Milliseconds()
}

// noteResponseShape records finish reason, upstream-reported model, and tool-call
// count.
func (tr *traceRecorder) noteResponseShape(stopReason, responseModel string, toolCalls int) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if stopReason != "" {
		tr.stopReason = stopReason
	}
	if responseModel != "" {
		tr.responseModel = responseModel
	}
	if toolCalls > 0 {
		tr.toolCalls = toolCalls
	}
}

// noteUsage records token/credit detail. Tokens are kept split so the trace can
// answer "was this an input-heavy or output-heavy request", while RequestLog.Tokens
// keeps carrying the sum for existing consumers.
func (tr *traceRecorder) noteUsage(inputTokens, outputTokens, cacheReadTokens int, credits float64) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.inputTokens = inputTokens
	tr.outputTokens = outputTokens
	tr.cacheReadTokens = cacheReadTokens
	tr.credits = credits
}

// markCacheHit flags a response served from the in-process response cache. Cache
// hits consume tenant quota, so they must appear in the logs rather than being
// invisible as they were before.
func (tr *traceRecorder) markCacheHit() {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.cacheHit = true
}

// finish materialises the accumulated trace as a RequestLog. outcome is one of
// the outcome* constants; httpStatus is what the client received.
func (tr *traceRecorder) finish(outcome string, httpStatus int) RequestLog {
	if tr == nil {
		return RequestLog{}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()

	entry := RequestLog{
		Time:            time.Now().Unix(),
		Endpoint:        tr.api,
		API:             tr.api,
		Model:           tr.model,
		Stream:          tr.stream,
		RequestID:       tr.traceID,
		ApiKeyID:        tr.apiKeyID,
		Duration:        time.Since(tr.startedAt).Milliseconds(),
		TTFBMs:          tr.ttfbMs,
		StopReason:      tr.stopReason,
		ResponseModel:   tr.responseModel,
		ToolCallCount:   tr.toolCalls,
		AttemptCount:    len(tr.attempts),
		Outcome:         outcome,
		HTTPStatus:      httpStatus,
		InputTokens:     tr.inputTokens,
		OutputTokens:    tr.outputTokens,
		CacheReadTokens: tr.cacheReadTokens,
		Tokens:          tr.inputTokens + tr.outputTokens,
		Credits:         tr.credits,
		CacheHit:        tr.cacheHit,
	}

	// Status stays success/error only, because account health and usage-anomaly
	// aggregation both switch on it. Outcome carries the finer detail.
	if outcome == outcomeSuccess || outcome == outcomeCacheHit {
		entry.Status = "success"
	} else {
		entry.Status = "error"
	}

	if len(tr.attempts) > 0 {
		entry.Attempts = append([]TraceAttempt(nil), tr.attempts...)
		// Request-level routing context comes from the attempt that actually
		// served (or last tried) the request, not the first one.
		last := tr.attempts[len(tr.attempts)-1]
		entry.AccountID = last.AccountID
		entry.AccountEmail = last.AccountEmail
		entry.Region = last.Region
		entry.ProfileArn = last.ProfileArn
		entry.UpstreamHost = last.UpstreamHost
		if last.Outcome == outcomeError {
			entry.ErrorType = last.ErrorType
			entry.Error = last.Error
		}
	}
	return entry
}

// emitTrace materialises the recorder and appends exactly one RequestLog for the
// client request, keeping the global counters in step with what is logged.
//
// Counters and logs used to disagree: cache hits bumped the counters without
// producing any log row, and a failover chain produced multiple rows for one
// request. Routing every terminal path through here keeps them reconcilable.
func (h *Handler) emitTrace(tr *traceRecorder, outcome string, httpStatus int) {
	if h == nil || tr == nil {
		return
	}
	entry := tr.finish(outcome, httpStatus)
	if entry.AccountEmail == "" && entry.AccountID != "" {
		entry.AccountEmail = requestLogAccountEmail(entry.AccountID)
	}

	// Failure counting lives here because this is the single terminal path for
	// a failed request. The success counters are NOT touched: successes are
	// already counted by recordSuccessForApiKey on the serving path, and
	// double-counting them would inflate totalRequests.
	if outcome == outcomeError {
		atomic.AddInt64(&h.totalRequests, 1)
		atomic.AddInt64(&h.failedRequests, 1)
	}

	h.appendRequestLog(entry)
}

// rejectionErrorType maps a rejection reason to a stable error-type bucket so
// the logs view can group rejections the same way it groups upstream errors.
func rejectionErrorType(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "rpm", "tpm", "rate_limit":
		return "rate_limit"
	default:
		return "auth"
	}
}

// recordRejection logs a request refused before it ever reached an upstream
// account: a bad/disabled API key, or a rate-limit denial.
//
// These paths return from middleware, so previously they produced no trace
// record at all — a tenant hammering with an invalid key or sitting on a 429 all
// day generated zero evidence. The record deliberately carries NO credential
// material: apiKeyID is scrubbed, because the caller may pass an unresolved
// value that is actually the offending secret.
func (h *Handler) recordRejection(api, apiKeyID, reason string, httpStatus int) {
	if h == nil {
		return
	}
	safeKeyID := scrubTraceText(strings.TrimSpace(apiKeyID))
	entry := RequestLog{
		Time:       time.Now().Unix(),
		Endpoint:   api,
		API:        api,
		RequestID:  newTraceID(),
		ApiKeyID:   safeKeyID,
		Status:     "error",
		Outcome:    outcomeRejected,
		HTTPStatus: httpStatus,
		ErrorType:  rejectionErrorType(reason),
		Error:      scrubTraceText(strings.TrimSpace(reason)),
	}
	h.appendRequestLog(entry)
}

// rejectedApiKeyLabel derives a non-identifying label for the credential that
// was refused.
//
// When authentication succeeded far enough to resolve an entry we use its ID.
// Otherwise the only thing available is the offending secret itself, so we emit
// a suffix-only fingerprint. config.MaskApiKey is deliberately NOT used here:
// it preserves the first 6 characters, which for a rejected key is real
// credential prefix an operator never needs in a log row.
func rejectedApiKeyLabel(r *http.Request) string {
	if r == nil {
		return ""
	}
	if id := apiKeyIDFromContext(r.Context()); id != "" {
		return id
	}
	provided := strings.TrimSpace(extractProvidedKey(r))
	if provided == "" {
		return "none"
	}
	if len(provided) <= 4 {
		return "invalid:****"
	}
	return "invalid:****" + provided[len(provided)-4:]
}

// auditLogClear records that an operator destroyed request-log evidence.
// Clearing is irreversible, so the action itself must leave a trail.
func (h *Handler) auditLogClear(clearedCount int) {
	if h == nil {
		return
	}
	h.appendAuditLog(AuditLog{
		Category: "logs",
		Action:   "clear",
		Status:   "success",
		SafeDetails: map[string]string{
			"clearedCount": strconv.Itoa(clearedCount),
		},
	})
}

// effectiveTraceRegion reports the data-plane region a request will actually be
// dispatched to: the override wins when pinned, then the ARN's embedded region,
// then the account's auth region. Recording the auth region alone would be
// misleading, since an IAM Identity Center account's portal region is routinely
// different from the region hosting its profile.
func effectiveTraceRegion(account *config.Account) string {
	if account == nil {
		return ""
	}
	if override := strings.TrimSpace(account.RegionOverride); override != "" {
		return override
	}
	if region := regionFromProfileArn(account.ProfileArn); region != "" {
		return region
	}
	return strings.TrimSpace(account.Region)
}
