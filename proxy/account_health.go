package proxy

// Account health score (F1): a rolling, per-account heuristic that aggregates
// request-log outcomes over a recent window into a 0-100 score, so operators can
// spot a degrading credential before it hard-bans.
//
// HONESTY NOTE: this is a HEURISTIC health score, not a validated ban predictor.
// AWS does not publish pre-ban signals, so we cannot claim to predict a ban. We
// score observable degradation (dangerous-error rate + consecutive streak) and
// surface it. Do not present this as "ban prediction" until validated against a
// real corpus of pre-ban error samples.
//
// Error-class weighting: some error types are ROUTINE and must NOT tank health:
//   - quota / overage      -> benign: the account hit its limit, entirely normal.
// Others indicate the credential itself is in trouble:
//   - auth / suspended / profile -> dangerous: token/permission/account problems
//     that precede or accompany a ban.
//   - unknown              -> mild: counts, but weighted low.

import "sort"

// Health tiers.
const (
	HealthTierHealthy  = "healthy"  // score >= healthyThreshold
	HealthTierDegraded = "degraded" // score in [criticalThreshold, healthyThreshold)
	HealthTierCritical = "critical" // score < criticalThreshold
	HealthTierUnknown  = "unknown"  // no requests in the window -> nothing to score
)

const (
	healthHealthyThreshold  = 70
	healthCriticalThreshold = 40

	// healthMinRequests is the minimum number of window requests before a score
	// is meaningful. Below this we report UNKNOWN rather than tanking an account
	// on one or two early errors.
	healthMinRequests = 5
)

// healthInput carries the aggregated per-account outcomes for a rolling window.
type healthInput struct {
	TotalRequests        int // all requests in the window (success + error)
	DangerousErrors      int // auth / suspended / profile
	MildErrors           int // unknown
	ConsecutiveDangerous int // trailing streak of dangerous errors (most recent first)
	// Benign errors (quota/overage) are intentionally NOT an input: they must not
	// affect health.
}

// scoreAccountHealth converts window outcomes into a 0-100 score and a tier.
// Pure and unit-testable. Higher is healthier.
func scoreAccountHealth(in healthInput) (int, string) {
	if in.TotalRequests < healthMinRequests {
		return 0, HealthTierUnknown
	}

	score := 100.0

	// Dangerous-error rate is the dominant term: a credential throwing auth /
	// suspended / profile errors is in real trouble. Full dangerous rate -> -70.
	dangerRate := float64(in.DangerousErrors) / float64(in.TotalRequests)
	score -= dangerRate * 70.0

	// Mild (unknown) errors nudge the score but never dominate: full mild rate -> -15.
	mildRate := float64(in.MildErrors) / float64(in.TotalRequests)
	score -= mildRate * 15.0

	// A consecutive streak of dangerous errors is the strongest degradation
	// signal (a credential failing every recent call). Each consecutive dangerous
	// error costs 12, capped at -48 (4 in a row = almost certainly broken).
	streak := in.ConsecutiveDangerous
	if streak > 4 {
		streak = 4
	}
	score -= float64(streak) * 12.0

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	s := int(score + 0.5)
	switch {
	case s >= healthHealthyThreshold:
		return s, HealthTierHealthy
	case s >= healthCriticalThreshold:
		return s, HealthTierDegraded
	default:
		return s, HealthTierCritical
	}
}

// isDangerousErrorType reports whether an error class indicates the credential
// itself is degrading (as opposed to a routine quota limit).
func isDangerousErrorType(errType string) bool {
	switch errType {
	case "auth", "suspended", "profile":
		return true
	default:
		return false
	}
}

// isBenignErrorType reports whether an error class is routine and must not affect
// health scoring (the account hit its usage limit).
func isBenignErrorType(errType string) bool {
	switch errType {
	case "quota", "overage":
		return true
	default:
		return false
	}
}

// aggregateHealth walks an account's request logs within [now-windowSeconds, now]
// and produces the health input. `logs` may span all accounts; only entries whose
// AccountID matches are considered. Order-independent: it sorts by time to compute
// the trailing consecutive-dangerous streak correctly.
func aggregateHealth(logs []RequestLog, accountID string, now, windowSeconds int64) healthInput {
	cutoff := now - windowSeconds
	var scoped []RequestLog
	for _, l := range logs {
		if l.AccountID != accountID {
			continue
		}
		if l.Time < cutoff {
			continue
		}
		scoped = append(scoped, l)
	}
	sort.Slice(scoped, func(i, j int) bool { return scoped[i].Time < scoped[j].Time })

	in := healthInput{}
	for _, l := range scoped {
		// Benign errors (quota/overage) count as neither a total-eligible failure
		// nor a success drag: exclude them entirely so a rate-limited account is
		// not marked unhealthy.
		if l.Status == "error" && isBenignErrorType(l.ErrorType) {
			continue
		}
		in.TotalRequests++
		if l.Status == "error" {
			if isDangerousErrorType(l.ErrorType) {
				in.DangerousErrors++
			} else {
				in.MildErrors++
			}
		}
	}

	// Trailing consecutive dangerous streak (walk newest -> oldest, skipping benign).
	for i := len(scoped) - 1; i >= 0; i-- {
		l := scoped[i]
		if l.Status == "error" && isBenignErrorType(l.ErrorType) {
			continue
		}
		if l.Status == "error" && isDangerousErrorType(l.ErrorType) {
			in.ConsecutiveDangerous++
			continue
		}
		break
	}

	return in
}
