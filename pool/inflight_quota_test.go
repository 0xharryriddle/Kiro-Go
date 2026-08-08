package pool

import (
	"kiro-go/config"
	"path/filepath"
	"sync"
	"testing"
)

// Tests for in-flight quota accounting (inflight_quota.go / PROPOSAL B1).
//
// The defect these lock in: the quota gate compared acc.UsageCurrent against
// acc.UsageLimit, and UsageCurrent is only written by an upstream refresh on a
// ~30-minute cycle. Between refreshes the router could not see its own
// consumption, so it kept dispatching to an account that was already capped
// (measured on the live fleet: 112 HTTP 402 cap errors in 8 minutes, 134
// wasted dispatches).

// initConfigForTest points config at a temp file so these tests never touch the
// real data/config.json (which holds live credentials).
func initConfigForTest(t *testing.T) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

// newTestPoolDrained builds a pool and guarantees the detached stats-persistence
// goroutines started by UpdateStats are drained before the harness tears down
// the temp config dir.
//
// UpdateStats writes config OFF the request path in a goroutine tracked by
// AccountPool.pendingWrites (see the struct comment). Every test here calls
// UpdateStats, so without this drain a late Save() re-creates config.json inside
// the t.TempDir that testing is removing, and the test fails with
// "TempDir RemoveAll cleanup: directory not empty" — an order-dependent flake
// that passes in isolation and fails in the full suite.
//
// Registered AFTER config.Init's t.TempDir cleanup so LIFO ordering runs this
// drain FIRST, before the directory is removed.
func newTestPoolDrained(t *testing.T, accounts ...config.Account) *AccountPool {
	t.Helper()
	pool := newTestPool(accounts...)
	t.Cleanup(pool.WaitForPendingWrites)
	return pool
}

// TestInFlightUsageParksAccountBeforeUpstreamRefresh is the core behavioural
// test: an account one credit short of its limit must stop being routable as
// soon as the proxy itself observes that credit, WITHOUT waiting for the
// upstream figure to catch up.
func TestInFlightUsageParksAccountBeforeUpstreamRefresh(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{ID: "nearly", Enabled: true, UsageCurrent: 99, UsageLimit: 100},
	)

	// Baseline: routable, because 99 < 100.
	if got := p.GetNext(); got == nil || got.ID != "nearly" {
		t.Fatalf("precondition failed: expected account to start routable, got %#v", got)
	}

	// The proxy serves a request costing 1 credit. UsageCurrent stays at 99 —
	// that is exactly the staleness window this feature exists to close.
	p.UpdateStats("nearly", 1000, 1)

	if got := p.GetNext(); got != nil {
		t.Fatalf("account should be parked once locally-observed usage reaches the limit "+
			"(upstream 99 + in-flight 1 >= 100), but it was still dispatched: %#v", got)
	}
}

// TestInFlightUsageDoesNotParkAccountWithHeadroom is the positive control for
// the test above. Without it, a gate that blocks EVERYTHING would pass the
// previous test and look like a working feature.
func TestInFlightUsageDoesNotParkAccountWithHeadroom(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{ID: "roomy", Enabled: true, UsageCurrent: 10, UsageLimit: 100},
	)

	p.UpdateStats("roomy", 1000, 5)

	if got := p.GetNext(); got == nil || got.ID != "roomy" {
		t.Fatalf("account with headroom (10+5 of 100) must stay routable, got %#v", got)
	}
	if delta := p.InFlightUsage("roomy"); delta != 5 {
		t.Fatalf("expected in-flight delta 5, got %v", delta)
	}
}

// TestInFlightUsageFailsOverToHealthySibling proves the routing consequence:
// the point is not merely to park the capped account but to keep serving the
// request from an account that can answer it.
func TestInFlightUsageFailsOverToHealthySibling(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{ID: "capped", Enabled: true, UsageCurrent: 99, UsageLimit: 100},
		config.Account{ID: "healthy", Enabled: true, UsageCurrent: 1, UsageLimit: 100},
	)

	p.UpdateStats("capped", 1000, 1)

	for i := 0; i < 5; i++ {
		got := p.GetNext()
		if got == nil {
			t.Fatalf("iteration %d: pool went dark despite a healthy sibling", i)
		}
		if got.ID == "capped" {
			t.Fatalf("iteration %d: dispatched to the capped account instead of failing over", i)
		}
	}
}

// TestUpstreamRefreshResetsInFlightDelta locks in the double-counting guard. A
// fresh UsageCurrent already includes the credits we tracked locally, so the
// delta must go to zero — otherwise every refresh cycle would stack our
// estimate on top of an upstream figure that already contains it and park
// healthy accounts permanently.
func TestUpstreamRefreshResetsInFlightDelta(t *testing.T) {
	initConfigForTest(t)
	if err := config.AddAccount(config.Account{
		ID: "acct", Enabled: true, UsageCurrent: 50, UsageLimit: 100,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPoolDrained(t)
	p.Reload() // adopt baseline UsageCurrent=50

	p.UpdateStats("acct", 1000, 20)
	if delta := p.InFlightUsage("acct"); delta != 20 {
		t.Fatalf("expected in-flight delta 20 before refresh, got %v", delta)
	}

	// Simulate an upstream refresh landing: usage moves 50 -> 70, which already
	// accounts for the 20 credits we observed. Written through
	// UpdateAccountInfo because that is the setter the real refresh path uses
	// (RefreshAccountInfo -> UpdateAccountInfo); UsageLimit must be passed too,
	// as that function writes it unconditionally and would otherwise zero the
	// limit and make this test pass for the wrong reason.
	if err := config.UpdateAccountInfo("acct", config.AccountInfo{
		UsageCurrent: 70,
		UsageLimit:   100,
	}); err != nil {
		t.Fatalf("UpdateAccountInfo: %v", err)
	}
	p.Reload()

	if delta := p.InFlightUsage("acct"); delta != 0 {
		t.Fatalf("upstream refresh must clear the in-flight delta (it is already "+
			"included in the new UsageCurrent), got %v — this would double-count", delta)
	}
	if got := p.GetNext(); got == nil || got.ID != "acct" {
		t.Fatalf("account at 70/100 after refresh must stay routable, got %#v", got)
	}
}

// TestInFlightDeltaSurvivesReloadWithUnchangedUsage is the counterpart control:
// a Reload that brings NO new upstream figure must not silently discard the
// delta. Reload runs on many triggers (account edits, admin actions), and
// clearing on every one would reopen the staleness window that B1 closes.
func TestInFlightDeltaSurvivesReloadWithUnchangedUsage(t *testing.T) {
	initConfigForTest(t)
	if err := config.AddAccount(config.Account{
		ID: "acct", Enabled: true, UsageCurrent: 99, UsageLimit: 100,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPoolDrained(t)
	p.Reload()
	p.UpdateStats("acct", 1000, 1)

	// A Reload with no upstream change at all (e.g. an unrelated config edit).
	p.Reload()

	if delta := p.InFlightUsage("acct"); delta != 1 {
		t.Fatalf("a Reload carrying no new upstream usage must preserve the delta, got %v", delta)
	}
	if got := p.GetNext(); got != nil {
		t.Fatalf("account must stay parked across an unrelated Reload, got %#v", got)
	}
}

// TestInFlightUsageRespectsOverageEnabled is a precedence control: an operator
// who has overage enabled upstream must never be gated by our local estimate.
// This mirrors isQuotaBlocked's own ordering.
func TestInFlightUsageRespectsOverageEnabled(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{
			ID: "overage", Enabled: true,
			UsageCurrent: 99, UsageLimit: 100, OverageStatus: "ENABLED",
		},
	)

	p.UpdateStats("overage", 1000, 50) // way past the limit

	if got := p.GetNext(); got == nil || got.ID != "overage" {
		t.Fatalf("overage-enabled account must stay routable regardless of in-flight usage, got %#v", got)
	}
}

// TestInFlightUsageRespectsAllowOverUsage is the same precedence control for the
// global operator toggle.
func TestInFlightUsageRespectsAllowOverUsage(t *testing.T) {
	initConfigForTest(t)
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateAllowOverUsage(false) })

	p := newTestPoolDrained(t,
		config.Account{ID: "acct", Enabled: true, UsageCurrent: 99, UsageLimit: 100},
	)
	p.UpdateStats("acct", 1000, 50)

	if got := p.GetNext(); got == nil || got.ID != "acct" {
		t.Fatalf("allowOverUsage=true must keep the account routable, got %#v", got)
	}
}

// TestInFlightUsageIgnoresAccountsWithoutLimitData is a control for the
// no-quota-data case: accounts with UsageLimit<=0 carry no limit to compare
// against, and a delta must not invent one.
func TestInFlightUsageIgnoresAccountsWithoutLimitData(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{ID: "nolimit", Enabled: true, UsageCurrent: 0, UsageLimit: 0},
	)
	p.UpdateStats("nolimit", 1000, 9999)

	if got := p.GetNext(); got == nil || got.ID != "nolimit" {
		t.Fatalf("account with no usable limit data must stay routable, got %#v", got)
	}
}

// TestInFlightUsageRejectsNonPositiveCredits guards the direction of the
// estimate. A zero/negative figure (a billing bug, or a frame carrying no
// credit) must never REDUCE observed usage, or it could un-park a capped
// account.
func TestInFlightUsageRejectsNonPositiveCredits(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{ID: "acct", Enabled: true, UsageCurrent: 99, UsageLimit: 100},
	)

	p.UpdateStats("acct", 1000, 1) // park it
	if got := p.GetNext(); got != nil {
		t.Fatalf("precondition failed: account should be parked, got %#v", got)
	}

	p.UpdateStats("acct", 1000, -50) // must not un-park
	if delta := p.InFlightUsage("acct"); delta != 1 {
		t.Fatalf("negative credits must not reduce the observed delta, got %v", delta)
	}
	if got := p.GetNext(); got != nil {
		t.Fatalf("negative credits must not un-park a capped account, got %#v", got)
	}
}

// TestDiagnosticsReportsInFlightQuotaExhaustion locks the operator surface to
// the routing truth. Reporting an account as available while every routing path
// skips it sends an operator hunting a fault that does not exist.
func TestDiagnosticsReportsInFlightQuotaExhaustion(t *testing.T) {
	initConfigForTest(t)
	if err := config.AddAccount(config.Account{
		ID: "acct", Enabled: true, UsageCurrent: 99, UsageLimit: 100,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newTestPoolDrained(t)
	p.Reload()
	p.UpdateStats("acct", 1000, 1)

	diags := p.Diagnostics()
	if len(diags) == 0 {
		t.Fatal("expected diagnostics for the configured account")
	}
	var found bool
	for _, d := range diags {
		if d.ID != "acct" {
			continue
		}
		found = true
		if d.Available {
			t.Fatalf("diagnostics reports the account available while routing parks it (reason=%q)", d.Reason)
		}
		if d.Reason != "quota_exhausted" {
			t.Fatalf("expected reason quota_exhausted, got %q", d.Reason)
		}
	}
	if !found {
		t.Fatal("no diagnostics entry for account acct")
	}
}

// TestInFlightDeltaPrunedForRemovedAccount guards against unbounded map growth,
// mirroring the prune Reload already does for cooldowns/breakers/health.
func TestInFlightDeltaPrunedForRemovedAccount(t *testing.T) {
	initConfigForTest(t)
	if err := config.AddAccount(config.Account{
		ID: "gone", Enabled: true, UsageCurrent: 1, UsageLimit: 100,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newTestPoolDrained(t)
	p.Reload()
	p.UpdateStats("gone", 1000, 3)
	if delta := p.InFlightUsage("gone"); delta != 3 {
		t.Fatalf("precondition failed: expected delta 3, got %v", delta)
	}

	if err := config.DeleteAccount("gone"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	p.Reload()

	if delta := p.InFlightUsage("gone"); delta != 0 {
		t.Fatalf("delta for a removed account must be pruned, got %v", delta)
	}
	p.mu.RLock()
	_, stillTracked := p.lastSeenUsage["gone"]
	p.mu.RUnlock()
	if stillTracked {
		t.Fatal("lastSeenUsage still tracks a removed account (unbounded growth)")
	}
}

// TestInFlightUsageIsRaceFree exercises the accounting under concurrent
// dispatch, which is how it actually runs. Meaningful under -race.
func TestInFlightUsageIsRaceFree(t *testing.T) {
	initConfigForTest(t)
	p := newTestPoolDrained(t,
		config.Account{ID: "a", Enabled: true, UsageCurrent: 0, UsageLimit: 1000000},
		config.Account{ID: "b", Enabled: true, UsageCurrent: 0, UsageLimit: 1000000},
	)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "a"
			if i%2 == 1 {
				id = "b"
			}
			p.UpdateStats(id, 10, 1)
			_ = p.GetNext()
			_ = p.InFlightUsage(id)
			_ = p.Diagnostics()
		}(i)
	}
	wg.Wait()
	p.WaitForPendingWrites()

	if got := p.InFlightUsage("a") + p.InFlightUsage("b"); got != 40 {
		t.Fatalf("expected 40 total observed credits across both accounts, got %v", got)
	}
}
