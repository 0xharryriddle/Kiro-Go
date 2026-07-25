package proxy

import (
	"encoding/csv"
	"io"
	"strconv"
)

// traceCSVHeader is the summary export layout. The original 11 columns are kept
// in their original positions so an existing spreadsheet or script that consumes
// this export keeps working; the trace fields are appended after them.
var traceCSVHeader = []string{
	// Original columns (order preserved for backward compatibility).
	"time", "endpoint", "model", "accountId", "accountEmail", "status", "errorType", "error",
	"tokens", "credits", "durationMs",
	// Trace additions.
	"traceId", "api", "outcome", "httpStatus", "stream", "apiKeyId",
	"attemptCount", "inputTokens", "outputTokens", "cacheReadTokens",
	"stopReason", "responseModel", "toolCallCount", "ttfbMs",
	"region", "profileArn", "upstreamHost", "cacheHit", "bodyRef", "bodyTruncated",
}

// writeTraceCSV streams the summary export row by row.
//
// Rows are written straight to the ResponseWriter rather than being buffered into
// one big slice first, so exporting a large window does not materialise the whole
// corpus in memory.
func writeTraceCSV(w io.Writer, logs []RequestLog) {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write(traceCSVHeader)
	for _, log := range logs {
		_ = cw.Write([]string{
			strconv.FormatInt(log.Time, 10),
			log.Endpoint,
			log.Model,
			log.AccountID,
			log.AccountEmail,
			log.Status,
			log.ErrorType,
			log.Error,
			strconv.Itoa(log.Tokens),
			strconv.FormatFloat(log.Credits, 'f', 6, 64),
			strconv.FormatInt(log.Duration, 10),

			log.RequestID,
			log.API,
			log.Outcome,
			strconv.Itoa(log.HTTPStatus),
			strconv.FormatBool(log.Stream),
			log.ApiKeyID,
			strconv.Itoa(log.AttemptCount),
			strconv.Itoa(log.InputTokens),
			strconv.Itoa(log.OutputTokens),
			strconv.Itoa(log.CacheReadTokens),
			log.StopReason,
			log.ResponseModel,
			strconv.Itoa(log.ToolCallCount),
			strconv.FormatInt(log.TTFBMs, 10),
			log.Region,
			log.ProfileArn,
			log.UpstreamHost,
			strconv.FormatBool(log.CacheHit),
			log.BodyRef,
			strconv.FormatBool(log.BodyTruncated),
		})
	}
}

// traceAttemptsCSVHeader is the per-attempt export layout, joined to the summary
// export by traceId.
var traceAttemptsCSVHeader = []string{
	"traceId", "time", "api", "model", "seq",
	"accountId", "accountEmail", "region", "profileArn",
	"upstreamEndpoint", "upstreamHost", "upstreamRequestId",
	"httpStatus", "outcome", "errorType", "error", "retryAfter",
	"startedAtMs", "durationMs",
}

// writeTraceAttemptsCSV emits one row per upstream attempt.
//
// This is the export that makes a failover chain analysable: the summary export
// has one row per client request, so a request that tried three accounts collapses
// to a single line and the per-account quota errors are invisible in it.
func writeTraceAttemptsCSV(w io.Writer, logs []RequestLog) {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write(traceAttemptsCSVHeader)
	for _, log := range logs {
		api := log.API
		if api == "" {
			api = log.Endpoint
		}
		// A record with no attempts (cache hit, rejection) still gets one row so
		// the two exports stay reconcilable by traceId.
		if len(log.Attempts) == 0 {
			_ = cw.Write([]string{
				log.RequestID, strconv.FormatInt(log.Time, 10), api, log.Model, "0",
				log.AccountID, log.AccountEmail, log.Region, log.ProfileArn,
				"", log.UpstreamHost, "",
				strconv.Itoa(log.HTTPStatus), log.Outcome, log.ErrorType, log.Error, "",
				"0", strconv.FormatInt(log.Duration, 10),
			})
			continue
		}
		for _, a := range log.Attempts {
			_ = cw.Write([]string{
				log.RequestID, strconv.FormatInt(log.Time, 10), api, log.Model, strconv.Itoa(a.Seq),
				a.AccountID, a.AccountEmail, a.Region, a.ProfileArn,
				a.UpstreamEndpoint, a.UpstreamHost, a.UpstreamRequestID,
				strconv.Itoa(a.HTTPStatus), a.Outcome, a.ErrorType, a.Error, a.RetryAfter,
				strconv.FormatInt(a.StartedAtMs, 10), strconv.FormatInt(a.DurationMs, 10),
			})
		}
	}
}
