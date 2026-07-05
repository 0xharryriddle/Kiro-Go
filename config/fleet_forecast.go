package config

// Fleet capacity forecast: project when an account's remaining period quota will
// be exhausted at its observed burn rate, so an operator can answer "do I need
// more accounts, and when?" This is a READ-ONLY projection — it changes no
// routing and takes no action.
//
// Unit: all figures are AWS agentic-request credits (the same unit as
// UsageCurrent/UsageLimit), the scarce resource this proxy manages.
//
// Burn rate is derived from the credits consumed since the period baseline
// (PeriodStart captured at PeriodStartAt) up to the latest observation. We use
// the AUTHORITATIVE upstream delta (UsageCurrent - PeriodStart), not just the
// credits we drove, because the account is exhausted by TOTAL consumption from
// every client, not only ours. This makes the forecast honest even when the
// credential is shared/used externally.

import "math"

// ForecastConfidence tiers describe how much to trust an account's projection.
const (
	ForecastConfidenceOK       = "ok"       // enough elapsed time + positive burn -> projection meaningful
	ForecastConfidenceIdle     = "idle"     // no measurable burn this period -> effectively never exhausts
	ForecastConfidenceUnknown  = "unknown"  // no quota data, no baseline, or too little elapsed time to project
	ForecastConfidenceDepleted = "depleted" // already at/over the limit
)

// forecastMinElapsedSeconds is the minimum time since the period baseline before
// a burn-rate projection is considered meaningful. Below this, a couple of early
// requests would extrapolate to an absurdly short runway, so we report unknown.
const forecastMinElapsedSeconds = 600 // 10 minutes

// AccountForecastInput carries the observed values needed to project one account.
type AccountForecastInput struct {
	UsageCurrent  float64 // upstream CurrentUsage (credits consumed this period, all clients)
	UsageLimit    float64 // upstream period limit (credits)
	PeriodStart   float64 // UsageCurrent captured at the period baseline
	PeriodStartAt int64   // Unix seconds when PeriodStart was captured
	HasUpstream   bool    // whether upstream usage data exists at all
	Enabled       bool    // whether the account currently routes locally
}

// AccountForecast is the per-account projection result.
type AccountForecast struct {
	RemainingCredits float64 `json:"remainingCredits"`         // clamped >= 0
	BurnPerHour      float64 `json:"burnPerHour"`              // credits/hour since baseline (total consumption)
	HoursToExhaust   float64 `json:"hoursToExhaust,omitempty"` // remaining / burn; 0 when not projectable
	ExhaustsAt       int64   `json:"exhaustsAt,omitempty"`     // Unix seconds projection; 0 when not projectable
	ElapsedSeconds   int64   `json:"elapsedSeconds"`           // time since baseline
	Confidence       string  `json:"confidence"`               // ok | idle | unknown | depleted
}

// ForecastAccount projects one account's exhaustion time. Pure: given observed
// values and the current time, it returns the projection. `now` must be >=
// PeriodStartAt for a meaningful elapsed window.
func ForecastAccount(in AccountForecastInput, now int64) AccountForecast {
	out := AccountForecast{Confidence: ForecastConfidenceUnknown}

	// No usable quota data -> cannot project.
	if !in.HasUpstream || in.UsageLimit <= 0 {
		return out
	}

	remaining := in.UsageLimit - in.UsageCurrent
	if remaining < 0 {
		remaining = 0
	}
	out.RemainingCredits = remaining

	// Already exhausted.
	if remaining == 0 {
		out.Confidence = ForecastConfidenceDepleted
		return out
	}

	// No baseline timestamp yet (fresh import, pre-first-rollover) -> cannot
	// compute a rate.
	if in.PeriodStartAt <= 0 || now <= in.PeriodStartAt {
		return out
	}

	elapsed := now - in.PeriodStartAt
	out.ElapsedSeconds = elapsed

	// Too little elapsed time to extrapolate honestly.
	if elapsed < forecastMinElapsedSeconds {
		return out
	}

	consumed := in.UsageCurrent - in.PeriodStart
	if consumed <= 0 {
		// No measurable burn this period -> effectively never exhausts at this rate.
		out.Confidence = ForecastConfidenceIdle
		return out
	}

	burnPerSecond := consumed / float64(elapsed)
	out.BurnPerHour = burnPerSecond * 3600.0

	secondsToExhaust := remaining / burnPerSecond
	out.HoursToExhaust = secondsToExhaust / 3600.0
	out.ExhaustsAt = now + int64(math.Round(secondsToExhaust))
	out.Confidence = ForecastConfidenceOK
	return out
}
