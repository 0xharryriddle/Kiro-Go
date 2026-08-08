package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

// Tests for overage backoff sizing (overage_backoff.go / PROPOSAL B2).
//
// Round 16 wired 402/overage into MarkOverLimit, which parked the account for a
// flat 1h. B2 sizes that from the upstream reset date — but clamped, because on
// real data the unclamped form the proposal described is WORSE than the flat hour
// (26/41 live accounts carry a reset date already in the past → zero-second park;
// the only enabled one sits 24 days out → outage).

// ---------------------------------------------------------------------------
// overageBackoffFor: the sizing rule itself (pure, deterministic `now`)
// ---------------------------------------------------------------------------

func TestOverageBackoffFallsBackToFlatHourOnUnusableResetDate(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		date string
	}{
		// The measured majority case: 26/41 live accounts carry 2026-08-01 while
		// the clock reads 2026-08-08. Sizing from it directly yields a NEGATIVE
		// duration, and setCooldownIfLater treats a past expiry as nothing to do —
		// so the 402'd account would have been parked for zero seconds.
		{"stale date seven days in the past", "2026-08-01"},
		{"today (midnight already passed)", "2026-08-08"},
		{"empty", ""},
		{"unparseable", "not-a-date"},
		{"wrong layout", "08/09/2026"},
		{"whitespace only", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, usable := overageBackoffFor(config.Account{NextResetDate: tc.date}, now)
			if usable {
				t.Fatalf("reset date %q must not be treated as usable", tc.date)
			}
			if got != minOverageBackoff {
				t.Fatalf("expected the flat %v fallback for %q, got %v",
					minOverageBackoff, tc.date, got)
			}
		})
	}
}

// The outage guard. The only currently-ENABLED account in the live fleet sits 24
// days from its reset date; parking it that long on a single 402 is an outage, not
// a backoff.
func TestOverageBackoffClampsFarFutureResetDate(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	acc := config.Account{NextResetDate: "2026-09-01"} // ~24 days out

	got, usable := overageBackoffFor(acc, now)
	if !usable {
		t.Fatal("a future reset date should be usable")
	}
	if got != maxOverageBackoff {
		t.Fatalf("expected clamp to %v, got %v (24 days would withhold the account)",
			maxOverageBackoff, got)
	}
}

// Between the floor and the ceiling the real distance is used — this is the actual
// feature, and without this test the clamps alone would pass everything.
func TestOverageBackoffUsesRealDistanceInsideClamp(t *testing.T) {
	now := time.Date(2026, 8, 8, 18, 0, 0, 0, time.UTC)
	acc := config.Account{NextResetDate: "2026-08-09"} // midnight UTC = 6h away

	got, usable := overageBackoffFor(acc, now)
	if !usable {
		t.Fatal("a future reset date should be usable")
	}
	if got != 6*time.Hour {
		t.Fatalf("expected the real 6h distance, got %v", got)
	}
}

// A reset that is imminent must still cost at least round 16's hour, or a 402
// arriving just before midnight would barely park the account at all.
func TestOverageBackoffNeverGoesBelowFlatHour(t *testing.T) {
	now := time.Date(2026, 8, 8, 23, 30, 0, 0, time.UTC)
	acc := config.Account{NextResetDate: "2026-08-09"} // 30 minutes away

	got, usable := overageBackoffFor(acc, now)
	if !usable {
		t.Fatal("a future reset date should be usable")
	}
	if got != minOverageBackoff {
		t.Fatalf("expected floor of %v, got %v", minOverageBackoff, got)
	}
}

// ---------------------------------------------------------------------------
// MarkOverLimit wiring
// ---------------------------------------------------------------------------

func markOverLimitCooldown(t *testing.T, acc config.Account) (time.Duration, time.Time) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.lastDispatchSeq = make(map[string]uint64)
	p.Reload()

	before := time.Now()
	p.MarkOverLimit(acc.ID)

	p.mu.RLock()
	cd, ok := p.cooldowns[acc.ID]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("MarkOverLimit left no cooldown at all")
	}
	return cd.Sub(before), cd
}

// The feature: a usable reset date inside the clamp window sizes the park.
func TestMarkOverLimitSizesBackoffFromResetDate(t *testing.T) {
	// Pick a reset date ~6h out relative to real `now`, since MarkOverLimit reads
	// the wall clock. Using tomorrow's date and asserting a window keeps this
	// robust regardless of the hour the suite runs at.
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	got, _ := markOverLimitCooldown(t, config.Account{
		ID: "hot", Enabled: true, UsageLimit: 100, UsageCurrent: 100,
		OverageStatus: "ENABLED", NextResetDate: tomorrow,
	})

	if got < minOverageBackoff {
		t.Fatalf("backoff %v is below the %v floor", got, minOverageBackoff)
	}
	if got > maxOverageBackoff+time.Minute {
		t.Fatalf("backoff %v exceeds the %v ceiling", got, maxOverageBackoff)
	}
}

// The regression guard that the measured data demands: a stale (past) reset date
// must not weaken round 16's hour. This is the case 26 of 41 live accounts are in.
func TestMarkOverLimitKeepsFlatHourOnStaleResetDate(t *testing.T) {
	got, _ := markOverLimitCooldown(t, config.Account{
		ID: "hot", Enabled: true, UsageLimit: 100, UsageCurrent: 100,
		OverageStatus: "ENABLED", NextResetDate: "2020-01-01",
	})

	if got < minOverageBackoff-time.Minute {
		t.Fatalf("a past reset date shortened the backoff to %v; round 16's %v floor must hold",
			got, minOverageBackoff)
	}
	if got > minOverageBackoff+time.Minute {
		t.Fatalf("a past reset date should size to exactly the %v fallback, got %v",
			minOverageBackoff, got)
	}
}

// The outage guard, end to end through MarkOverLimit.
func TestMarkOverLimitClampsFarFutureResetDate(t *testing.T) {
	far := time.Now().UTC().Add(90 * 24 * time.Hour).Format("2006-01-02")
	got, _ := markOverLimitCooldown(t, config.Account{
		ID: "hot", Enabled: true, UsageLimit: 100, UsageCurrent: 100,
		OverageStatus: "ENABLED", NextResetDate: far,
	})

	if got > maxOverageBackoff+time.Minute {
		t.Fatalf("a 90-day reset date parked the account for %v; the %v ceiling must hold",
			got, maxOverageBackoff)
	}
}

// An account with no usable limit data still gets the flat hour — sizing must not
// depend on quota fields being present.
func TestMarkOverLimitParksAccountWithoutQuotaData(t *testing.T) {
	got, _ := markOverLimitCooldown(t, config.Account{
		ID: "hot", Enabled: true, OverageStatus: "ENABLED",
	})
	if got < minOverageBackoff-time.Minute {
		t.Fatalf("expected at least the %v floor, got %v", minOverageBackoff, got)
	}
}

// ---------------------------------------------------------------------------
// releaseOnPeriodRollover: the valve that makes a long park safe
// ---------------------------------------------------------------------------

// reloadPoolWithUsage seeds one account, adopts its baseline, applies a cooldown,
// then lands a new upstream usage figure and reloads so the change is observed.
func reloadPoolWithUsage(t *testing.T, first, second float64) *AccountPool {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "acct", Enabled: true, UsageLimit: 1000, UsageCurrent: first,
		OverageStatus: "ENABLED", // stay routable so Reload keeps it in the pool
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.Reload() // first sighting: adopt `first` as the baseline

	p.mu.Lock()
	p.cooldowns["acct"] = time.Now().Add(8 * time.Hour)
	p.errorCounts["acct"] = 2
	p.mu.Unlock()

	// UpdateAccountInfo is the setter the real refresh path uses; UsageLimit must
	// be passed too because it is written unconditionally.
	if err := config.UpdateAccountInfo("acct", config.AccountInfo{
		UsageCurrent: second, UsageLimit: 1000,
	}); err != nil {
		t.Fatalf("UpdateAccountInfo: %v", err)
	}
	p.Reload() // observe the change
	return p
}

// A usage DROP is a billing-period reset: the park must be released immediately
// rather than held to the (now much longer) backoff expiry.
func TestPeriodRolloverReleasesOverageBackoff(t *testing.T) {
	p := reloadPoolWithUsage(t, 900, 0)

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["acct"]
	errs := p.errorCounts["acct"]
	p.mu.RUnlock()

	if hasCooldown {
		t.Fatal("usage dropped (period reset) but the overage cooldown was still held — " +
			"a 12h backoff would withhold an account whose quota already reset")
	}
	if errs != 0 {
		t.Fatalf("expected the consecutive-error count to reset on rollover, got %d", errs)
	}
}

// The control that stops the above from being satisfied by "clear on any change".
// Usage RISING is ordinary consumption, not a reset, and must not un-park a
// capped account — that would resurrect the exact defect B1 closed.
func TestUsageIncreaseDoesNotReleaseOverageBackoff(t *testing.T) {
	p := reloadPoolWithUsage(t, 500, 700)

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["acct"]
	p.mu.RUnlock()

	if !hasCooldown {
		t.Fatal("rising usage cleared the cooldown; only a DROP indicates a period reset")
	}
}

// Unchanged usage must likewise leave the park alone (the Reload-with-no-news case).
func TestUnchangedUsageDoesNotReleaseOverageBackoff(t *testing.T) {
	p := reloadPoolWithUsage(t, 600, 600)

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["acct"]
	p.mu.RUnlock()

	if !hasCooldown {
		t.Fatal("a Reload carrying no usage change cleared the cooldown")
	}
}
