package proxy

import (
	"net/http"
	"time"
)

// Passthrough trace emission (PROPOSAL D1).
//
// WHAT WAS ACTUALLY WRONG. The proposal said the Bedrock paths "emit no
// structured trace row". Measured against the code, that is not true: a Bedrock
// request DOES produce a row via recordBedrockSuccess -> recordSuccessLog ->
// appendRequestLog. The real defect is narrower and, in one respect, worse:
//
//  1. The row is the LEGACY minimal shape. recordSuccessLog fills only
//     {Time, Endpoint, Model, AccountID, ApiKeyID, Status, Tokens, Credits,
//     Duration}. Everything the trace UI and CSV export read is absent:
//     RequestID, Outcome, API, Stream, HTTPStatus, Attempts, the input/output
//     token split, cache tokens, TTFB, StopReason, ResponseModel, ToolCallCount,
//     Region, ProfileArn, UpstreamHost, AccountEmail, BodyRef.
//
//  2. A traceRecorder was ALREADY allocated for the request, and an attempt was
//     ALREADY opened against the serving account (handler.go: tr at 2020/3001/
//     3370/3847, beginAttempt at 2054/3008/3377/3854) — then the passthrough
//     branch returned without endAttempt or emitTrace. So the recorder was built
//     and thrown away. Two consequences: the row carries no RequestID, so it
//     cannot be joined to anything; and a failover chain that ENDS on a
//     passthrough account discards the attempt history of every account that
//     failed before it, which is exactly the evidence an operator needs.
//
//  3. It is not Bedrock-specific. recordCustomApiSuccess
//     (custom_api_forward.go) funnels into the same recordSuccessLog, so every
//     transparent passthrough shares the defect. Fixing only Bedrock would
//     leave the sibling path broken in the identical way.
//
// WHY THIS IS A SWAP, NOT AN ADDITION. emitTrace also ends in
// appendRequestLog (request_trace_recorder.go:384), so calling both would write
// TWO rows for one request — the precise failure emitTrace's own doc comment
// says it exists to prevent ("a failover chain produced multiple rows for one
// request"). Hence recordPassthroughTrace emits exactly one row and the callers
// no longer call recordSuccessLog.
//
// Counters are unaffected by the swap, which is what makes it safe:
// recordSuccessLog only appends a row, and emitTrace bumps counters only on
// outcomeError. Success counters keep coming from recordSuccessForApiKey on the
// serving path, exactly as before.

// passthroughUsage is the metering detail a passthrough knows about its own
// response. Grouped into a struct because the emit path needs six values and a
// six-parameter call is where argument-order bugs live.
type passthroughUsage struct {
	inputTokens  int
	outputTokens int

	// cacheReadTokens/cacheWriteTokens are the portion of inputTokens that came
	// from (or was written to) the prompt cache — a BREAKDOWN of inputTokens, not
	// an addition to it. This matches both the Kiro path (kiro.go: inputTokens =
	// uncached + cacheRead + cacheWrite) and the Bedrock native extractor
	// (bedrockUsageTokens.totalInput), so a reader can compare rows across
	// providers without knowing which produced them.
	//
	// Only set these where the upstream actually reports them. Leaving them zero
	// on a path that cannot measure caching is a known limitation, not a claim:
	// see the note on recordBedrockSuccess about the Converse API.
	cacheReadTokens  int
	cacheWriteTokens int

	credits float64
}

// recordPassthroughTrace closes the caller's attempt and emits exactly one
// RequestLog for a successful passthrough request.
//
// When no recorder was threaded through (p.trace == nil) it falls back to the
// legacy thin row, so callers that do not trace — bedrockTestReply, the admin
// account test, and every test constructing forwardParams by hand — keep
// working and keep producing a log line. That fallback is the reason this is
// safe to drop into eight call sites at once.
func (h *Handler) recordPassthroughTrace(p forwardParams, endpoint string, u passthroughUsage, reqStart time.Time) {
	if h == nil {
		return
	}
	if p.trace == nil {
		// No trace context: preserve the pre-D1 behaviour exactly.
		h.recordSuccessLog(endpoint, p.model, accountIDOf(p), p.apiKeyID,
			u.inputTokens+u.outputTokens, u.credits, time.Since(reqStart).Milliseconds())
		return
	}

	// Close the attempt the dispatch loop opened. Without this the served
	// account never appears in Attempts, so a row could report a failover chain
	// whose FINAL, successful hop is missing — the one an operator most wants.
	// endAttempt is nil-safe, so a caller that threaded a recorder but no attempt
	// still emits a complete request-level row.
	p.trace.endAttempt(p.attempt, nil)

	p.trace.noteUsage(u.inputTokens, u.outputTokens, u.cacheReadTokens, u.cacheWriteTokens, u.credits)
	h.emitTrace(p.trace, outcomeSuccess, http.StatusOK)
}

// notePassthroughFailedAttempt closes an attempt that failed before the client
// received anything, so the account that just failed stays in the trace even
// though this request will be retried elsewhere.
//
// It deliberately does NOT emit a row: the request is not over. The dispatch
// loop either succeeds on a later account (which emits one row carrying every
// attempt) or exhausts the pool and reports the failure through its own tail.
// Emitting here would produce one row per attempt, which is the row-inflation
// emitTrace was introduced to remove.
func (h *Handler) notePassthroughFailedAttempt(p forwardParams, err error) {
	if p.trace == nil || err == nil {
		return
	}
	p.trace.endAttempt(p.attempt, err)
}

// accountIDOf is nil-safe so the fallback path cannot panic on a forwardParams
// built without an account (admin probes, tests).
func accountIDOf(p forwardParams) string {
	if p.account == nil {
		return ""
	}
	return p.account.ID
}
