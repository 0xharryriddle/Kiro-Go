package pool

import (
	"errors"
	"testing"
)

// The 5xx gate in IsAuthFailure must key off a status that is actually presented
// AS an HTTP status, not off any bare three-digit number that happens to fall in
// 400-599.
//
// Error strings routinely carry unrelated integers in that range: credit and
// usage counters ("usage 512/1000 credits"), elapsed seconds ("expired 540
// seconds ago"), balances, sequence numbers. Reading one of those as a server
// status inverts the classifier: a genuinely revoked credential gets filed as an
// upstream outage, so the account is never flagged for re-auth and keeps being
// routed while every request fails.
//
// That is the exact mirror of the false-ban the 5xx gate exists to prevent, and
// it is worse in one respect: a false ban is visible in the admin UI, whereas a
// missed revocation looks like a healthy account that merely keeps erroring.
//
// proxy's isAuthErrorMessage already had this right — its patterns require HTTP
// context ("HTTP 500", "refresh failed: 500", "(status 500)", "upstream returned
// 500"). This pins the same requirement on the pool sibling.
func TestIsAuthFailureIgnoresBareNumbersThatAreNotStatuses(t *testing.T) {
	credentialFailures := []string{
		// Bare in-range integers that are NOT statuses. Each carries a genuine
		// credential marker that must still win.
		"unauthorized (usage 512/1000 credits)",
		"invalid_grant: token expired 540 seconds ago",
		"bad credentials; remaining balance 550",
		"token has expired [seq 501]",
		"invalid_token; request id 478",
		"invalid_grant after 599 retries",
	}
	for _, msg := range credentialFailures {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("credential failure missed because a bare number was read as a server status:\n  %s", msg)
		}
	}
}

// The gate must still fire when the 5xx really is presented as a status, in every
// format this repo's formatters emit.
func TestIsAuthFailureStillGatesRealServerStatuses(t *testing.T) {
	outages := []string{
		"HTTP 500 from kiro: {\"error\":\"invalid_grant\"}",
		"refresh failed: 503 <html>unauthorized</html>",
		"social token exchange failed (status 502): invalid_token",
		"upstream returned 500: token expired",
		"upstream status 504: bad credentials",
	}
	for _, msg := range outages {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("upstream outage misclassified as a credential failure (would permanently ban):\n  %s", msg)
		}
	}
}

// And a real 401/403 must still be detected regardless of surrounding noise.
func TestIsAuthFailureStillCatchesContextualAuthStatuses(t *testing.T) {
	authFailures := []string{
		"HTTP 401 from kiro: denied",
		"HTTP 403 from kiro: forbidden",
		"refresh failed: 401 {\"error\":\"invalid_grant\"}",
		"social token exchange failed (status 403): denied",
		// Authoritative 401 whose body quotes a 5xx: the header must win.
		"refresh failed: 401 {\"error\":\"invalid_grant\",\"trace\":\"HTTP 503 from edge\"}",
	}
	for _, msg := range authFailures {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("genuine credential failure not detected:\n  %s", msg)
		}
	}
}
