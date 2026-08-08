package pool

import "kiro-go/config"

// In-flight quota accounting (PROPOSAL B1 / roadmap P0-1).
//
// THE PROBLEM, measured on the live fleet. The quota gate is
// isQuotaBlocked -> isOverUsageLimit(acc), which compares acc.UsageCurrent
// against acc.UsageLimit. UsageCurrent is only ever written by an upstream
// refresh (RefreshAccountInfo -> config.UpdateAccountUsage), and background
// refresh runs on a ~30-minute cycle. Between two refreshes the value does not
// move no matter how much traffic the proxy sends, so the number the router
// makes decisions with is up to half an hour stale. Observed consequence: 112
// HTTP 402 MONTHLY_REQUEST_COUNT errors inside 8 minutes on one account, i.e.
// 134 dispatches to an account already known to be capped.
//
// THE SIGNAL ALREADY EXISTS AND WAS BEING DISCARDED. UpdateStats runs after
// every successful request and receives `credits` — the upstream's own
// consumption figure for that request, parsed from the event stream
// (kiro.go OnCredits). It was accumulated into Account.TotalCredits (a
// LIFETIME counter, used for reporting) and never reached the period-scoped
// quota gate.
//
// WHY THIS ADDS A DELTA INSTEAD OF REPLACING UsageCurrent. Verified against
// data/config.json across 41 real accounts: for accounts whose traffic is
// proxy-only the two counters agree to ~1 unit (xuanan-nguyen usageCurrent
// 3876 vs totalCredits 3874.98; ducdung-vu 3870 vs 3870.88), which is what
// proves `credits` and `UsageCurrent` share a unit (agentic requests, ~1-2
// per request — NOT tokens; credits/1k_tokens is ~0.005-0.026, so treating
// them as tokens would under-count by ~100x). But other accounts diverge
// hugely in the other direction — user.brandon.garcia usageCurrent 4882 vs
// totalCredits 1957, noor.holmes usageCurrent 2486 with no local counter at
// all — because the same Kiro account is also used by the Kiro IDE and other
// clients. Local credits are therefore a LOWER BOUND on period usage, never
// the whole truth: the upstream figure stays authoritative and this only adds
// what we know has happened since it was captured.
//
// SAFETY DIRECTION. The delta can only ever make an account look MORE used,
// never less, so the worst case is parking an account slightly early — which
// costs one failover to a healthy sibling. The failure it removes is the
// opposite and much worse: dispatching into an upstream that already returns
// 402, which burns a request slot, a retry, and client latency for nothing.

// noteUsageDelta records `credits` of period usage observed locally for an
// account since its last upstream refresh. Caller must hold p.mu.
//
// Called from UpdateStats, which is the single point every successful request
// already funnels through — so there is no second accounting path to keep in
// sync, and any future caller of UpdateStats gets in-flight accounting for
// free.
func (p *AccountPool) noteUsageDelta(id string, credits float64) {
	// A zero or negative figure carries no information. Negative would be a
	// billing bug upstream; either way, never let it *reduce* observed usage,
	// because that would let a bad frame un-park a capped account.
	if id == "" || credits <= 0 {
		return
	}
	if p.usageDelta == nil {
		p.usageDelta = make(map[string]float64)
	}
	p.usageDelta[id] += credits
}

// syncUsageBaselines drops the local delta for every account whose upstream
// UsageCurrent has changed since we last saw it. Caller must hold p.mu.
//
// This is deliberately driven by OBSERVING the value rather than by having
// each refresh site call a reset hook. There are several paths that write
// UsageCurrent (background refresh, the admin refresh endpoint, the API-key
// batch importer, per-account probes), they live in a different package, and a
// new one added later would silently double-count for a full period if it
// forgot the hook. Comparing the value cannot be forgotten: a fresh
// UsageCurrent already includes the credits we had been tracking, so the delta
// has served its purpose and must go to zero.
//
// Called from Reload, which is where the pool re-reads config, so the sync
// happens exactly when a new upstream value can first become visible.
func (p *AccountPool) syncUsageBaselines(accounts []config.Account) {
	if p.lastSeenUsage == nil {
		p.lastSeenUsage = make(map[string]float64)
	}
	seen := make(map[string]bool, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		seen[acc.ID] = true
		prev, had := p.lastSeenUsage[acc.ID]
		if !had {
			// First sighting: adopt the value as the baseline. Do NOT clear the
			// delta here — an account can appear in a Reload (for example the
			// first Reload after a restart, or after being re-enabled) while
			// requests are already in flight against it, and clearing would
			// throw away real observed usage.
			p.lastSeenUsage[acc.ID] = acc.UsageCurrent
			continue
		}
		if prev != acc.UsageCurrent {
			// A DROP is what a billing-period reset looks like from here, and it
			// is the release valve that makes B2's multi-hour overage backoff safe
			// (see overage_backoff.go): without it an account whose quota genuinely
			// reset would stay parked until the backoff expired, trading wasted
			// dispatches for withheld capacity. Checked before the baseline is
			// overwritten, since that is what makes the drop visible at all.
			if acc.UsageCurrent < prev {
				p.releaseOnPeriodRollover(acc.ID)
			}
			p.lastSeenUsage[acc.ID] = acc.UsageCurrent
			delete(p.usageDelta, acc.ID)
		}
	}
	// Prune state for accounts that left config, mirroring what Reload already
	// does for cooldowns/errorCounts/healthStats. Without this the maps grow
	// for the lifetime of the process.
	for id := range p.lastSeenUsage {
		if !seen[id] {
			delete(p.lastSeenUsage, id)
		}
	}
	for id := range p.usageDelta {
		if !seen[id] {
			delete(p.usageDelta, id)
		}
	}
}

// effectiveUsage returns the account's best-known period usage: the last
// upstream figure plus locally-observed consumption since then. Caller must
// hold at least p.mu.RLock.
func (p *AccountPool) effectiveUsage(acc config.Account) float64 {
	return acc.UsageCurrent + p.usageDelta[acc.ID]
}

// quotaBlocked is the pool's in-flight-aware quota gate and the only one the
// router should use. It mirrors isQuotaBlocked exactly, except that the usage
// side of the comparison includes the local delta.
//
// The free function isQuotaBlocked is kept for callers with no pool in scope
// and as the definition of the upstream-only rule; every routing path in this
// package goes through this method instead. Caller must hold at least RLock.
func (p *AccountPool) quotaBlocked(acc config.Account, allowOverUsage bool) bool {
	if acc.UsageLimit <= 0 {
		// No usable limit data: the upstream-only rule already declines to
		// block these, and a delta cannot make a limitless account over-limit.
		return false
	}
	if isUpstreamOverageEnabled(acc) || allowOverUsage {
		// Overage is permitted, so being over the limit is not a block. Same
		// precedence as isQuotaBlocked — checked before the usage comparison so
		// an operator who enables overage is never gated by our own estimate.
		return false
	}
	return p.effectiveUsage(acc) >= acc.UsageLimit
}

// InFlightUsage exposes the locally-observed delta for an account, for
// diagnostics and admin surfaces. Returns 0 for unknown accounts.
func (p *AccountPool) InFlightUsage(id string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.usageDelta[id]
}
