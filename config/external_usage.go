package config

// External-usage audit: detect whether an account credential is being used
// OUTSIDE this proxy (real Kiro IDE, another sharer, a second proxy) by comparing
// upstream billing-period usage growth against the credits WE metered in the same
// period.
//
// Unit: both upstream CurrentUsage and our per-request meteringEvent.usage are the
// same AWS metering unit (agentic-request credits). There is deliberately NO token
// figure here: upstream exposes no token count for traffic we did not originate, so
// any "external tokens" number would be fabricated. This audit is credits-only.
//
// Model (period-scoped, drift-safe):
//
//	externalDelta = (upstreamCurrent - periodStart) - ourPeriodCredits
//
// We accumulate signed and clamp to >=0 only at display, so metering lag (we posted
// a credit upstream hasn't accounted yet) cancels out over the period instead of
// biasing external upward window-by-window.

// External-usage confidence tiers.
const (
	ExternalConfidenceClean          = "clean"           // external within tolerance -> only we use this credential
	ExternalConfidenceExternal       = "external"        // external materially positive -> used elsewhere too
	ExternalConfidenceStrongExternal = "strong_external" // disabled for local routing yet upstream grew -> unambiguous
	ExternalConfidenceUnknown        = "unknown"         // no upstream data, fresh period baseline, or just reset
)

// externalCreditsFloor is the minimum absolute tolerance (in credits) below which a
// positive gap is treated as metering noise rather than real external usage. Prevents
// flapping to "external" on sub-credit rounding when period growth is tiny.
const externalCreditsFloor = 1.0

// externalToleranceFraction is the share of in-period upstream growth we allow as
// slack before flagging external usage, on top of the absolute floor.
const externalToleranceFraction = 0.05

// ExternalUsageInput carries the freshly-observed values needed to recompute an
// account's external-usage estimate for the current billing period.
type ExternalUsageInput struct {
	PeriodKey       string  // current billing period identifier (NextResetDate); "" when unknown
	UpstreamCurrent float64 // upstream CurrentUsage for the period
	HasUpstream     bool    // whether upstream usage data is available at all
	EnabledLocally  bool    // account.Enabled — whether we currently route to it
}

// ExternalUsageState is the mutable per-account accumulator (a subset of Account
// fields) that ComputeExternalUsage reads and returns updated.
type ExternalUsageState struct {
	PeriodKey       string
	PeriodStart     float64
	PeriodStartAt   int64 // Unix seconds when PeriodStart baseline was captured (for burn-rate forecast)
	PeriodOurCredit float64
	Estimate        float64
	Confidence      string
	CheckedAt       int64
}

// ComputeExternalUsage recomputes the external-usage verdict for one account. It is
// pure: given the previous accumulator state, a fresh observation, and the current
// time, it returns the next state. Callers persist the result.
//
// Rollover: when the billing period changes (PeriodKey differs) or upstream usage
// drops below our recorded baseline (a reset we missed), we re-baseline to the
// current upstream value, zero the in-period accumulator, and report UNKNOWN for
// that one cycle — usage from a prior period is never miscounted as external, and
// we never emit a verdict from an incomplete baseline.
func ComputeExternalUsage(prev ExternalUsageState, in ExternalUsageInput, now int64) ExternalUsageState {
	out := prev
	out.CheckedAt = now

	// No upstream data (never refreshed, or a mid-period import with no baseline):
	// we cannot make any honest claim.
	if !in.HasUpstream || in.PeriodKey == "" {
		out.Confidence = ExternalConfidenceUnknown
		return out
	}

	// Period rollover or an unseen reset: re-baseline, no verdict this cycle.
	if prev.PeriodKey != in.PeriodKey || in.UpstreamCurrent < prev.PeriodStart {
		out.PeriodKey = in.PeriodKey
		out.PeriodStart = in.UpstreamCurrent
		out.PeriodStartAt = now
		out.PeriodOurCredit = 0
		out.Estimate = 0
		out.Confidence = ExternalConfidenceUnknown
		return out
	}

	upstreamDelta := in.UpstreamCurrent - prev.PeriodStart
	if upstreamDelta < 0 {
		upstreamDelta = 0
	}
	external := upstreamDelta - prev.PeriodOurCredit
	if external < 0 {
		external = 0
	}
	out.Estimate = external

	tolerance := upstreamDelta * externalToleranceFraction
	if tolerance < externalCreditsFloor {
		tolerance = externalCreditsFloor
	}

	switch {
	case external <= tolerance:
		out.Confidence = ExternalConfidenceClean
	case !in.EnabledLocally:
		// We are not routing to this credential now, yet upstream grew beyond what
		// we drove earlier in the period: unambiguous third-party consumption.
		out.Confidence = ExternalConfidenceStrongExternal
	default:
		out.Confidence = ExternalConfidenceExternal
	}
	return out
}
