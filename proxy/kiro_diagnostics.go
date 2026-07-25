package proxy

import (
	"net/http"
	"strings"
)

// KiroCallDiagnostics reports per-endpoint upstream detail for one CallKiroAPI
// invocation. It is populated even when the call ultimately fails, which is the
// point: before this existed the only upstream signal that survived was a
// flattened error string, so the HTTP status code and any upstream correlation
// ID were unrecoverable from a log record.
//
// Endpoints holds one entry per endpoint tried during fan-out. A single
// CallKiroAPI can try several endpoints, so this is a slice rather than a single
// value.
type KiroCallDiagnostics struct {
	Endpoints []TraceAttempt
}

// record appends one endpoint attempt. Nil-safe so existing call sites can pass
// a nil sink and opt out of diagnostics entirely.
func (d *KiroCallDiagnostics) record(attempt TraceAttempt) {
	if d == nil {
		return
	}
	d.Endpoints = append(d.Endpoints, attempt)
}

// Last returns the most recent endpoint attempt, or nil when none were made.
func (d *KiroCallDiagnostics) Last() *TraceAttempt {
	if d == nil || len(d.Endpoints) == 0 {
		return nil
	}
	return &d.Endpoints[len(d.Endpoints)-1]
}

// upstreamRequestIDCandidates are the response headers that may carry an
// upstream correlation ID.
//
// Nothing in this codebase previously read any upstream response header except
// Retry-After, so the exact header Kiro returns is not established here. Rather
// than betting on one spelling, probe the known AWS/HTTP variants in priority
// order and take the first non-empty value. Whichever one the live endpoint
// actually sets will be captured.
var upstreamRequestIDCandidates = []string{
	"x-amzn-RequestId",
	"x-amzn-requestid",
	"x-amz-request-id",
	"x-amzn-trace-id",
	"x-request-id",
}

// canonicalHeaderKey exposes Go's header canonicalisation so tests can build a
// header map with the same keying http.Header.Get uses.
func canonicalHeaderKey(key string) string {
	return http.CanonicalHeaderKey(key)
}

// regionFromKiroHost extracts the AWS region from a Kiro/CodeWhisperer data-plane
// hostname such as "codewhisperer.eu-central-1.amazonaws.com".
//
// This records where a request was ACTUALLY dispatched, which can differ from the
// account's auth region — an IAM Identity Center account's portal region is
// routinely in a different region from the profile that serves its traffic, and
// that mismatch is exactly what a trace needs to make visible.
func regionFromKiroHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	parts := strings.Split(host, ".")
	for _, part := range parts {
		if looksLikeAWSRegion(part) {
			return part
		}
	}
	return ""
}

// looksLikeAWSRegion reports whether a hostname label has the shape of an AWS
// region ("us-east-1", "eu-central-1", "ap-southeast-2"): three hyphen-separated
// segments ending in a digit group.
func looksLikeAWSRegion(label string) bool {
	segments := strings.Split(label, "-")
	if len(segments) < 3 {
		return false
	}
	last := segments[len(segments)-1]
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, seg := range segments[:len(segments)-1] {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

// upstreamRequestIDFromHeader returns the first upstream correlation ID present.
func upstreamRequestIDFromHeader(h http.Header) string {
	if h == nil {
		return ""
	}
	for _, key := range upstreamRequestIDCandidates {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}
