package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
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
	case isAuthErrorMessage(msg):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
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
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
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
var upstreamStatusPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bhttp (\d{3})\b`),
	regexp.MustCompile(`(?i)refresh failed:\s*(\d{3})\b`),
	regexp.MustCompile(`(?i)\(status (\d{3})\)`),
}

// upstreamStatusFromMessage returns the HTTP status carried by an upstream error
// message and whether one was found.
func upstreamStatusFromMessage(lower string) (int, bool) {
	for _, re := range upstreamStatusPatterns {
		if m := re.FindStringSubmatch(lower); m != nil {
			if status, err := strconv.Atoi(m[1]); err == nil {
				return status, true
			}
		}
	}
	return 0, false
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
		// A definite non-auth status: trust it over whatever the body says,
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

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
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
