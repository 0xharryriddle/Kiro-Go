package proxy

import (
	"strings"

	"github.com/google/uuid"
)

// newTraceID returns a collision-free identifier for one client request. Every
// RequestLog produced for that request carries this value, so a failover chain,
// a cache hit, and a rejection are all joinable back to the caller's request.
func newTraceID() string {
	return "trc_" + uuid.New().String()
}

// Outcome values recorded on a RequestLog. Status stays "success"/"error" for
// backward compatibility with existing consumers (account health, usage anomaly,
// the CSV export and the current frontend); Outcome carries the finer detail.
const (
	outcomeSuccess  = "success"
	outcomeError    = "error"
	outcomeCacheHit = "cache_hit"
	outcomeRejected = "rejected"
)

// TraceAttempt is one upstream dispatch attempt within a single client request.
// A request that fails over across three accounts produces three attempts, so
// the quota error that caused a reroute stays visible even when the request as a
// whole succeeded.
type TraceAttempt struct {
	Seq               int    `json:"seq"`
	AccountID         string `json:"accountId,omitempty"`
	AccountEmail      string `json:"accountEmail,omitempty"`
	UpstreamEndpoint  string `json:"upstreamEndpoint,omitempty"`
	UpstreamHost      string `json:"upstreamHost,omitempty"`
	Region            string `json:"region,omitempty"`
	ProfileArn        string `json:"profileArn,omitempty"`
	HTTPStatus        int    `json:"httpStatus,omitempty"`
	UpstreamRequestID string `json:"upstreamRequestId,omitempty"`
	StartedAtMs       int64  `json:"startedAtMs"`
	DurationMs        int64  `json:"durationMs"`
	Outcome           string `json:"outcome"`
	ErrorType         string `json:"errorType,omitempty"`
	Error             string `json:"error,omitempty"`
	RetryAfter        string `json:"retryAfter,omitempty"`
}

// profileArnSuffix reduces a profile ARN to its trailing identifier so traces
// can show which profile served a request without echoing the full ARN (which
// embeds the AWS account ID) into every log row.
func profileArnSuffix(arn string) string {
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return ""
	}
	if idx := strings.LastIndex(arn, "/"); idx >= 0 && idx < len(arn)-1 {
		return arn[idx+1:]
	}
	return arn
}
