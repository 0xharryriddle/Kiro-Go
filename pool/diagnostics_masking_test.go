package pool

import (
	"testing"
	"time"

	"kiro-go/config"
)

// diagnosticsForLocked evaluates its reasons as an if/else-if chain, so the
// FIRST matching branch wins. token_refresh_due sat ahead of the quota and
// not_in_pool branches AND set available=true, so an account that is quota
// exhausted (and therefore dropped from the routing pool by Reload) was
// reported to the operator as Available=true / token_refresh_due purely because
// its token happened to be inside the refresh skew window.
//
// That is the admin panel actively misreporting state: the operator sees a
// healthy account while every request routed to it is rejected.
func TestDiagnosticsDoesNotMaskQuotaExhaustionAsTokenRefresh(t *testing.T) {
	p := newTestPool() // empty pool: Reload() dropped the exhausted account

	acc := config.Account{
		ID:           "exhausted",
		Enabled:      true,
		UsageCurrent: 100,
		UsageLimit:   100,
		// Inside the refresh skew window, which used to win the branch race.
		ExpiresAt: time.Now().Unix() + 30,
	}

	out := p.DiagnosticsFor([]config.Account{acc})
	if len(out) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(out))
	}
	got := out[0]

	if got.Available {
		t.Fatalf("quota-exhausted account reported Available=true (reason=%q): the admin panel would show it as healthy", got.Reason)
	}
	if got.Reason != "quota_exhausted" {
		t.Fatalf("reason = %q, want \"quota_exhausted\"", got.Reason)
	}
}

// Same masking applies to an account that is simply not in the routing pool
// (e.g. dropped for any reason) while its token is due for refresh.
func TestDiagnosticsDoesNotMaskNotInPoolAsTokenRefresh(t *testing.T) {
	p := newTestPool() // empty pool

	acc := config.Account{
		ID:        "missing",
		Enabled:   true,
		ExpiresAt: time.Now().Unix() + 30, // inside refresh skew
	}

	out := p.DiagnosticsFor([]config.Account{acc})
	if len(out) != 1 {
		t.Fatalf("expected 1 diagnostic")
	}
	if out[0].Available {
		t.Fatalf("account absent from the routing pool reported Available=true (reason=%q)", out[0].Reason)
	}
	if out[0].Reason != "not_in_pool" {
		t.Fatalf("reason = %q, want \"not_in_pool\"", out[0].Reason)
	}
}

// The genuine token_refresh_due case must be preserved: an account that IS in
// the pool with quota remaining and a token inside the skew window is still
// available, because the request path refreshes it before use. Regressing this
// to unavailable would make the panel report false outages.
func TestDiagnosticsKeepsGenuineTokenRefreshDueAvailable(t *testing.T) {
	acc := config.Account{
		ID:           "healthy",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   100,
		ExpiresAt:    time.Now().Unix() + 30, // inside refresh skew
	}
	p := newTestPool(acc) // account IS in the routing pool

	out := p.DiagnosticsFor([]config.Account{acc})
	if len(out) != 1 {
		t.Fatalf("expected 1 diagnostic")
	}
	if !out[0].Available {
		t.Fatalf("healthy refresh-due account reported unavailable (reason=%q)", out[0].Reason)
	}
	if out[0].Reason != "token_refresh_due" {
		t.Fatalf("reason = %q, want \"token_refresh_due\"", out[0].Reason)
	}
}
