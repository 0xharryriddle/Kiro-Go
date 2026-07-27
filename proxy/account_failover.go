package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const maxAccountRetryAttempts = 3

func isQuotaErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	// Match the 429 status code only as a digit-boundary token (parity with
	// pool.HasStatusToken used by the auth classifier) so a stray "429" inside
	// an upstream body token/ID can't false-trigger RecordError(true). "quota"
	// remains a word marker.
	return pool.HasStatusToken(lower, "429") || strings.Contains(lower, "quota")
}

func statusForUpstreamError(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	msg := err.Error()
	switch {
	case isQuotaErrorMessage(msg):
		return http.StatusTooManyRequests
	case isOverageErrorMessage(msg):
		return http.StatusPaymentRequired
	case isInputTooLongErrorMessage(msg):
		// A length rejection is a property of the REQUEST, not the server, so it
		// must surface as 400. Reporting 500 told the client the service had
		// failed and that retrying the identical oversized payload was
		// reasonable; 400 tells it to shrink the conversation instead.
		return http.StatusBadRequest
	case isAuthErrorMessage(msg):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// claudeStopReasonError is the stop_reason emitted when a stream is aborted
// mid-message by an upstream failure.
//
// It is deliberately OUTSIDE Anthropic's documented stop_reason enum
// (end_turn / max_tokens / stop_sequence / tool_use), and that is the point: a
// mid-stream abort is not a completion. Reporting end_turn would tell the client
// the message finished normally, so partial output would be treated as the whole
// answer — silent truncation, which is worse than an unknown enum value a client
// can branch on. The accompanying `error` event carries the classified detail.
const claudeStopReasonError = "error"

// claudeErrorTypeForStatus maps an authoritative upstream HTTP status onto
// Anthropic's documented error-type enum for the Claude surface.
//
// The mid-stream error event previously hardcoded "api_error" for every failure,
// while the OpenAI stream on the very same failure classified via
// errorTypeForOpenAIStatus. That asymmetry is client-visible and consequential,
// because an Anthropic consumer keys its retry policy off error.type:
//
//   - a rate limit reported as api_error invites an immediate retry into an
//     already-exhausted account instead of a backoff;
//   - a revoked credential reported as api_error looks transient, so a client
//     retries indefinitely rather than surfacing "re-authenticate";
//   - an oversized request reported as api_error implies the service failed,
//     when the correct client action is to shrink the conversation.
//
// Every return value is a type Anthropic actually defines. Inventing one would
// be worse than a generic api_error: a client switching on the enum would fall
// through to an unknown branch. Anything unrecognised therefore degrades to
// api_error rather than guessing.
func claudeErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusPaymentRequired:
		// 402 (overage/quota-cap) is a property of the REQUEST against the
		// account's plan, not a server fault, so it belongs here rather than in
		// api_error.
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		// 503 is the one 5xx with a distinct Anthropic type: it tells the client
		// to retry later, whereas api_error does not.
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func errorTypeForOpenAIStatus(status int) string {
	if status == http.StatusTooManyRequests {
		return "rate_limit_error"
	}
	if status == http.StatusUnauthorized {
		return "authentication_error"
	}
	return "server_error"
}

func applyRetryAfterHeader(w http.ResponseWriter, err error) {
	if w == nil || err == nil || !isQuotaErrorMessage(err.Error()) {
		return
	}
	if retryAfter := retryAfterFromError(err.Error()); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
		return
	}
	w.Header().Set("Retry-After", "60")
}

func retryAfterFromError(msg string) string {
	idx := strings.LastIndex(strings.ToLower(msg), "retry after ")
	if idx < 0 {
		return ""
	}
	value := strings.TrimSpace(msg[idx+len("retry after "):])
	if semi := strings.Index(value, ";"); semi >= 0 {
		value = strings.TrimSpace(value[:semi])
	}
	return value
}

func isOverageErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	// 402 must be a digit-boundary token, not an arbitrary substring (parity
	// with pool.HasStatusToken), ANDed with the "overage" word marker.
	return pool.HasStatusToken(lower, "402") && strings.Contains(lower, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

// isInputTooLongErrorMessage reports whether upstream rejected the request
// because the INPUT was too large, as opposed to anything about the account.
//
// This condition was previously named only in comments and never detected. That
// mattered once the body ceiling stopped being a single hardcoded number: the
// old flat 900KB cap was tuned by hand to sit under the observed threshold, so
// the error was assumed unreachable. With the ceiling now derived from each
// model's declared window, an over-estimate is possible, and an undetected
// length rejection is the worst outcome — it burns the request, counts as a
// generic failure against the account, and triggers failover to another account
// that will fail identically.
//
// Classifying it enables two correct behaviours: never blame the account (the
// credential is fine), and shrink-and-retry rather than failing over.
func isInputTooLongErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "content_length_exceeds_threshold") ||
		strings.Contains(lower, "input is too long") ||
		strings.Contains(lower, "prompt is too long") ||
		strings.Contains(lower, "too many tokens") ||
		strings.Contains(lower, "exceeds the maximum") ||
		strings.Contains(lower, "context length exceeded") ||
		strings.Contains(lower, "context_length_exceeded")
}

// upstreamStatusPatterns match the authoritative HTTP status in an upstream error
// string. The status is structural — the code that formats these errors puts it
// there — whereas everything after it is an opaque upstream response body.
//
// Three formats exist in the codebase and all three must be recognised, because a
// format that is NOT matched falls back to scanning the body and can ban a healthy
// account:
//
//	"HTTP 500 from kiro: <body>"                     proxy/kiro.go
//	"refresh failed: 500 <body>"                     auth/oidc.go
//	"social token exchange failed (status 503): ..."  auth/kiro_sso.go
//	"upstream status 502: <body>"                    proxy/bedrock.go
//	"upstream returned 502: <body>"                  generic forward/gateway errors
//
// The last family was NOT matched before, so a 502 whose HTML body happened to
// contain the word "forbidden" fell through to body-word scanning and
// permanently banned a healthy account (TestHandleAccountFailureDoesNotBanOnForbiddenInBody).
var upstreamStatusPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bhttp (\d{3})\b`),
	regexp.MustCompile(`(?i)refresh failed:\s*(\d{3})\b`),
	regexp.MustCompile(`(?i)\(status (\d{3})\)`),
	regexp.MustCompile(`(?i)\bupstream (?:returned|status)\s*:?\s*(\d{3})\b`),
}

// upstreamStatusFromMessage returns the HTTP status carried by an upstream error
// message and whether one was found.
//
// It returns the LEFTMOST match across all patterns, not the first pattern that
// happens to hit. That distinction is load-bearing. Every formatter in this repo
// writes the authoritative status at the FRONT of the message and appends the
// opaque upstream body after it, so position — not pattern order — is what
// separates "the status the upstream returned" from "a number that appears
// inside a response body".
//
// Selecting by pattern order instead let a body win over the header. For
//
//	refresh failed: 401 {"error":"invalid_grant","trace":"HTTP 503 from edge"}
//
// the `http (\d{3})` pattern is tried first and matched the body's 503, so the
// message was classified as a 5xx outage — and once isAuthErrorMessage refuses
// to honour markers on 5xx, a genuinely revoked credential stopped being
// recognised as an auth failure and the account was never flagged for re-auth.
// Anchoring on the earliest occurrence keeps the header authoritative regardless
// of what the body quotes.
func upstreamStatusFromMessage(lower string) (int, bool) {
	bestIdx := -1
	bestStatus := 0
	for _, re := range upstreamStatusPatterns {
		loc := re.FindStringSubmatchIndex(lower)
		if loc == nil {
			continue
		}
		// loc[0] is the start of the whole match; loc[2]:loc[3] is group 1.
		status, err := strconv.Atoi(lower[loc[2]:loc[3]])
		if err != nil {
			continue
		}
		if bestIdx < 0 || loc[0] < bestIdx {
			bestIdx = loc[0]
			bestStatus = status
		}
	}
	if bestIdx < 0 {
		return 0, false
	}
	return bestStatus, true
}

// authErrorNarrowMarkers are phrases specific enough that they identify a
// credential failure on their own, even without a 401/403 status: they name an
// OAuth/token grant condition rather than describing permissions in prose.
var authErrorNarrowMarkers = []string{
	"invalid_grant",
	"invalid grant",
	"invalid_token",
	"authentication failed",
	"access token expired",
	"refresh token expired",
}

// isAuthErrorMessage reports whether an upstream error means THIS account's
// credentials are bad. The answer is load-bearing: handleAccountFailure routes a
// true here to disableAccount(..., "BANNED", ...), a permanent ban only an
// operator can lift, and statusForUpstreamError turns it into a client-facing 401.
//
// It used to substring-match bare words ("unauthorized", "forbidden", "token
// expired") anywhere in the message. Because the message embeds the FULL upstream
// response body, any unrelated 5xx whose body merely mentioned one of those words
// — an upstream stack trace, a WAF/gateway HTML page, a JSON error about some
// other resource's permissions — permanently banned a perfectly healthy account
// and reported the outage to the client as an auth error.
//
// The status token is authoritative, so when one is present it decides; the body
// only gets a vote through the narrow markers above. Errors with no HTTP status
// (OAuth refresh failures and similar) keep the marker-based path.
func isAuthErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)

	if status, ok := upstreamStatusFromMessage(lower); ok {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return true
		}
		// 5xx is the upstream failing, never this account's credentials being
		// bad, and the narrow markers are NOT safe to honour here: the message
		// embeds an opaque upstream body (a stack trace, a gateway page, an IdP
		// error_description that copies a request marker), so a 500 whose body
		// merely mentions invalid_grant used to permanently BAN a healthy
		// account. A server error cannot revoke a credential, so no phrase found
		// inside one is evidence about the credential. Failover/retry still
		// happens via the non-auth path; only the irreversible ban is withheld.
		if status >= 500 {
			return false
		}
		// A definite non-auth 4xx: trust the status over whatever the body says,
		// but still honour a narrow marker (e.g. a 400 carrying invalid_grant
		// is a genuine revoked refresh token).
		return containsAny(lower, authErrorNarrowMarkers)
	}

	// No status to anchor on. Keep the original broader markers so credential
	// failures surfaced without an HTTP status are still caught.
	return containsAny(lower, authErrorNarrowMarkers) ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "token invalid") ||
		strings.Contains(lower, "token expired")
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// shouldRetryAccountRefreshOnError reports whether a RefreshAccountInfo error
// looks like a stale/invalid token worth one token-refresh + retry in the admin
// "refresh account" endpoint (handler.go). This is a RETRY trigger, NOT a ban
// classifier — a false positive only costs a redundant refresh + retry, so its
// markers are deliberately broader than isAuthErrorMessage's. Status codes
// 401/403 are matched by digit boundary (pool.HasStatusToken) for parity with the
// ban classifiers, so a stray digit in a request ID/token can't fire a spurious
// refresh+retry; "invalid"/"expired" remain word markers.
func shouldRetryAccountRefreshOnError(msg string) bool {
	lower := strings.ToLower(msg)
	return pool.HasStatusToken(lower, "401") || pool.HasStatusToken(lower, "403") ||
		strings.Contains(lower, "invalid") || strings.Contains(lower, "expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	if !account.Enabled && account.BanStatus == banStatus && account.BanReason == banReason {
		return
	}

	if err := config.SetAccountBanStatus(account.ID, banStatus, banReason); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isInputTooLongErrorMessage(errMsg):
		// The request was too large for the model. Nothing is wrong with this
		// account, and every other account in the pool would reject the same
		// payload identically — so this must NOT record an error against it.
		// Recording one would cool down a healthy account (and, after enough
		// oversized requests, walk the whole pool into cooldown) for a fault
		// that lives entirely in the request we built.
		//
		// It is listed first because an upstream length rejection can arrive as
		// a 400 whose body mentions tokens or limits, which later cases could
		// otherwise misread.
		logger.Warnf("[AccountFailover] Upstream rejected the request as too long for %s (account not penalised): %v",
			accountEmailForLog(account), err)
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isProfileOrPlanAuthzError(errMsg):
		// A 403 about the PROFILE/PLAN — e.g. "User is not authorized to make this
		// call" when the profileArn is missing or points at a not-in-plan profile —
		// is NOT a token ban. Permanently disabling here is exactly what wrongly
		// banned a valid Enterprise-IdP account whose profile simply had not resolved
		// yet. Treat it as a soft failure (short cooldown, no disable); the profile
		// resolver / self-heal recovers the correct profile on a subsequent call.
		logger.Warnf("[AccountFailover] Profile/plan authorization error for %s (not banning): %v", account.Email, err)
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		if account.IsKiroAPIKeyCredential() {
			// Kiro keys cannot self-heal via OAuth refresh, and one upstream
			// auth response may be transient. Rotate/cool down instead of
			// permanently banning the account on the first failure.
			logger.Warnf("[AccountFailover] Kiro API-key auth error for %s (not auto-banning): %v", account.Email, err)
			h.pool.RecordError(account.ID, false)
			return
		}
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
