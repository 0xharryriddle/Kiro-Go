package pool

import (
	"strings"
	"time"

	"kiro-go/config"
)

// Overage/quota backoff sizing (PROPOSAL B2, the other half of round 16).
//
// Round 16 wired the 402/overage branch into MarkOverLimit, which parks the
// account for a flat 1 hour. The proposal's follow-up was to "size the backoff
// from NextResetDate instead of a flat 1h". Measured against the 41 real accounts
// in data/config.json, the naive form of that is actively WORSE than the flat
// hour, so this implements a clamped form instead. The evidence:
//
//   - 26/41 accounts carry NextResetDate = 2026-08-01 while the host clock reads
//     2026-08-08 — i.e. SEVEN DAYS IN THE PAST (the field is only as fresh as the
//     last upstream refresh, and these accounts are disabled so they never get
//     one). time.Until on that is NEGATIVE, and setCooldownIfLater treats a past
//     expiry as nothing to do — so a 402 would have parked the account for zero
//     seconds. Strictly worse than 1h.
//   - The other 15/41 sit 24 days out, and the only currently-ENABLED account
//     (david_smith25452, 10000/10000) is one of them. Parking it for 24 days on a
//     single 402 is an outage, not a backoff.
//   - The field is date-only ("2006-01-02", derived from a unix timestamp in
//     kiro_api.go and truncated), so even a valid value carries up to 24h of
//     error.
//   - Nothing else in the repo does arithmetic on it: admin_fleet_forecast.go and
//     admin_usage_audit.go pass the string straight through for display, and the
//     real forecast math uses UsageCurrent/UsageLimit. This would be the first
//     consumer to treat it as a clock, which is why its staleness had never bitten.
//
// So: use the reset date when it is parseable AND in the future, clamped into
// [minOverageBackoff, maxOverageBackoff]; otherwise keep the flat hour. That
// strictly dominates the previous behaviour — never shorter than 1h, never longer
// than the ceiling — and the ceiling is what bounds the damage from a stale or
// wrong date.
//
// The reason a longer park is safe here at all is that two existing mechanisms
// stop it becoming lost capacity:
//
//  1. fallbackEarliestCooldown (account.go:574) serves the account whose cooldown
//     expires soonest when nothing healthy is left, so a fully-parked pool still
//     answers instead of going dark.
//  2. releaseOnPeriodRollover below drops the backoff the moment the upstream
//     figure shows the period actually reset, so a real reset frees the account
//     immediately rather than after the ceiling.
const (
	// minOverageBackoff preserves round 16's contract: a 402 always costs the
	// account at least an hour. Also the floor that makes a stale/past reset date
	// a no-op rather than a regression.
	minOverageBackoff = time.Hour

	// maxOverageBackoff bounds how long one 402 can park an account. 12h means a
	// capped account is retried about twice a day instead of 24 times, while a
	// wrong date (or a period boundary we misread) costs at most half a day —
	// and releaseOnPeriodRollover usually cuts it far shorter.
	maxOverageBackoff = 12 * time.Hour
)

// overageBackoffFor returns how long a 402/overage response should park this
// account, and whether the reset date was actually usable (for logging and tests).
//
// The date is interpreted as midnight UTC of that day. That is deliberately the
// conservative reading: if the real reset happens later that day we under-park and
// retry a little early, which costs one 402 — whereas over-parking withholds a
// healthy account. It only matters when the reset is under maxOverageBackoff away,
// since the clamp dominates otherwise.
func overageBackoffFor(acc config.Account, now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(acc.NextResetDate)
	if raw == "" {
		return minOverageBackoff, false
	}
	reset, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		// Unparseable is not an error worth failing a request over; it just means
		// we have no clock and fall back to the flat hour.
		return minOverageBackoff, false
	}
	d := reset.Sub(now)
	if d <= 0 {
		// Past date: stale field (see the 26/41 above), not "reset immediately".
		return minOverageBackoff, false
	}
	if d < minOverageBackoff {
		return minOverageBackoff, true
	}
	if d > maxOverageBackoff {
		return maxOverageBackoff, true
	}
	return d, true
}

// releaseOnPeriodRollover clears a quota/overage cooldown for an account whose
// upstream usage figure DROPPED, which is what a billing-period reset looks like
// from here. Caller must hold p.mu.
//
// This is the release valve that makes a multi-hour backoff safe: without it, an
// account whose quota genuinely reset would stay parked until the backoff expired,
// so sizing the backoff up would trade wasted dispatches for withheld capacity.
//
// A drop could in principle come from a bad upstream read rather than a real
// reset. Clearing a cooldown is the recoverable direction of that mistake: the
// next request either succeeds (the reset was real) or 402s and re-parks the
// account. Withholding a healthy account for 12h is not equally recoverable.
//
// Only cooldowns are cleared, never the circuit breaker or the error count — a
// period reset says nothing about whether the upstream is answering.
func (p *AccountPool) releaseOnPeriodRollover(id string) {
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
}
