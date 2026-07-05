package proxy

// Usage anomaly detection (F12): flag per-account usage spikes by comparing a
// short RECENT window's request/token rate against a longer BASELINE window's
// average rate. A spike is a recent rate that materially exceeds the account's
// own established baseline — an early signal of runaway usage, a leaked key
// driving one credential, or a misbehaving client.
//
// HONESTY NOTE: this is a HEURISTIC over observed request logs, not a security
// guarantee. It detects deviation from an account's OWN recent history; it does
// not know intent. It shares the request-log data source with the health score
// (F1) and reuses the same in-memory ring buffer — no new upstream calls, no
// new persistence.
//
// Rates are normalized per-hour so the short and long windows are comparable
// despite different widths.

import "sort"

// Anomaly tiers.
const (
	AnomalyTierNormal  = "normal"  // recent rate within tolerance of baseline
	AnomalyTierSpike   = "spike"   // recent rate materially exceeds baseline
	AnomalyTierUnknown = "unknown" // not enough baseline history to judge
)

const (
	// anomalyRecentSeconds is the trailing window whose rate we test for a spike.
	anomalyRecentSeconds int64 = 3600 // 1 hour
	// anomalyBaselineSeconds is the longer window establishing the "normal" rate.
	anomalyBaselineSeconds int64 = 24 * 3600 // 24 hours
	// anomalyMinBaselineRequests is the minimum baseline-window requests before a
	// spike verdict is meaningful; below this we report unknown.
	anomalyMinBaselineRequests = 10
	// anomalySpikeMultiplier is how many times the baseline hourly rate the recent
	// hourly rate must exceed to count as a spike.
	anomalySpikeMultiplier = 3.0
	// anomalyMinRecentRequests guards against flagging a spike on a single stray
	// request when the baseline rate is near zero.
	anomalyMinRecentRequests = 5
)

// anomalyInput carries the aggregated per-account rates for detection.
type anomalyInput struct {
	RecentRequests    int     // requests in the recent window
	BaselineRequests  int     // requests in the baseline window (includes recent)
	RecentRatePerHr   float64 // recent requests normalized per hour
	BaselineRatePerHr float64 // baseline requests normalized per hour
}

// detectUsageAnomaly converts aggregated rates into an anomaly verdict and a
// ratio (recent/baseline hourly rate). Pure and unit-testable.
func detectUsageAnomaly(in anomalyInput) (tier string, ratio float64) {
	if in.BaselineRequests < anomalyMinBaselineRequests || in.BaselineRatePerHr <= 0 {
		return AnomalyTierUnknown, 0
	}
	ratio = in.RecentRatePerHr / in.BaselineRatePerHr
	// Require both a real recent volume AND a rate that clears the multiplier, so
	// a couple of stray requests over a near-idle baseline don't false-flag.
	if in.RecentRequests >= anomalyMinRecentRequests && ratio >= anomalySpikeMultiplier {
		return AnomalyTierSpike, ratio
	}
	return AnomalyTierNormal, ratio
}

// aggregateAnomaly walks an account's request logs and computes recent/baseline
// request counts and per-hour rates. `logs` may span all accounts; only entries
// whose AccountID matches within the baseline window are considered. Both
// successes and errors count as usage (the concern is request volume).
func aggregateAnomaly(logs []RequestLog, accountID string, now int64) anomalyInput {
	recentCutoff := now - anomalyRecentSeconds
	baselineCutoff := now - anomalyBaselineSeconds

	var scoped []RequestLog
	for _, l := range logs {
		if l.AccountID != accountID || l.Time < baselineCutoff {
			continue
		}
		scoped = append(scoped, l)
	}
	sort.Slice(scoped, func(i, j int) bool { return scoped[i].Time < scoped[j].Time })

	in := anomalyInput{}
	for _, l := range scoped {
		in.BaselineRequests++
		if l.Time >= recentCutoff {
			in.RecentRequests++
		}
	}
	in.RecentRatePerHr = float64(in.RecentRequests) / (float64(anomalyRecentSeconds) / 3600.0)
	in.BaselineRatePerHr = float64(in.BaselineRequests) / (float64(anomalyBaselineSeconds) / 3600.0)
	return in
}
