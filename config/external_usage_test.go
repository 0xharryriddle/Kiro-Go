package config

import "testing"

// TestComputeExternalUsage_NoUpstream: without upstream data (never refreshed) we
// can make no claim and must report UNKNOWN, preserving prior accumulator fields.
func TestComputeExternalUsage_NoUpstream(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "2026-08-01", PeriodStart: 10, PeriodOurCredit: 5}
	out := ComputeExternalUsage(prev, ExternalUsageInput{HasUpstream: false}, 100)
	if out.Confidence != ExternalConfidenceUnknown {
		t.Fatalf("want unknown, got %q", out.Confidence)
	}
	if out.CheckedAt != 100 {
		t.Fatalf("want CheckedAt=100, got %d", out.CheckedAt)
	}
	// Accumulator must be preserved (we did not observe a new period).
	if out.PeriodStart != 10 || out.PeriodOurCredit != 5 {
		t.Fatalf("accumulator mutated: %+v", out)
	}
}

// TestComputeExternalUsage_EmptyPeriodKey: upstream present but no period key means
// no usable baseline -> UNKNOWN.
func TestComputeExternalUsage_EmptyPeriodKey(t *testing.T) {
	out := ComputeExternalUsage(ExternalUsageState{}, ExternalUsageInput{HasUpstream: true, PeriodKey: "", UpstreamCurrent: 42}, 1)
	if out.Confidence != ExternalConfidenceUnknown {
		t.Fatalf("want unknown, got %q", out.Confidence)
	}
}

// TestComputeExternalUsage_FreshBaseline: first observation of a period re-baselines
// to the current upstream value and reports UNKNOWN for that cycle (no verdict from
// an incomplete baseline).
func TestComputeExternalUsage_FreshBaseline(t *testing.T) {
	out := ComputeExternalUsage(ExternalUsageState{}, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "2026-08-01", UpstreamCurrent: 100, EnabledLocally: true,
	}, 1)
	if out.Confidence != ExternalConfidenceUnknown {
		t.Fatalf("want unknown on fresh baseline, got %q", out.Confidence)
	}
	if out.PeriodStart != 100 {
		t.Fatalf("want PeriodStart=100, got %v", out.PeriodStart)
	}
	if out.PeriodOurCredit != 0 || out.Estimate != 0 {
		t.Fatalf("want zeroed accumulator, got %+v", out)
	}
}

// TestComputeExternalUsage_Clean: upstream grew exactly by what we drove -> CLEAN.
func TestComputeExternalUsage_Clean(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "2026-08-01", PeriodStart: 100, PeriodOurCredit: 20}
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "2026-08-01", UpstreamCurrent: 120, EnabledLocally: true,
	}, 1)
	if out.Confidence != ExternalConfidenceClean {
		t.Fatalf("want clean, got %q (estimate=%v)", out.Confidence, out.Estimate)
	}
	if out.Estimate != 0 {
		t.Fatalf("want estimate 0, got %v", out.Estimate)
	}
}

// TestComputeExternalUsage_WithinTolerance: small gap under the 5%/floor tolerance
// stays CLEAN rather than flapping to external.
func TestComputeExternalUsage_WithinTolerance(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "p", PeriodStart: 0, PeriodOurCredit: 100}
	// upstream delta 101, we drove 100 -> external 1.0 == floor -> clean (<=).
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "p", UpstreamCurrent: 101, EnabledLocally: true,
	}, 1)
	if out.Confidence != ExternalConfidenceClean {
		t.Fatalf("want clean within tolerance, got %q (estimate=%v)", out.Confidence, out.Estimate)
	}
}

// TestComputeExternalUsage_External: upstream grew materially more than we drove
// while still enabled locally -> EXTERNAL.
func TestComputeExternalUsage_External(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "p", PeriodStart: 100, PeriodOurCredit: 10}
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "p", UpstreamCurrent: 160, EnabledLocally: true,
	}, 1)
	// upstream delta 60, ours 10 -> external 50, tolerance = max(60*0.05=3, 1)=3 -> external.
	if out.Confidence != ExternalConfidenceExternal {
		t.Fatalf("want external, got %q", out.Confidence)
	}
	if out.Estimate != 50 {
		t.Fatalf("want estimate 50, got %v", out.Estimate)
	}
}

// TestComputeExternalUsage_StrongExternal: account disabled for local routing yet
// upstream grew beyond what we drove -> STRONG_EXTERNAL (unambiguous).
func TestComputeExternalUsage_StrongExternal(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "p", PeriodStart: 100, PeriodOurCredit: 0}
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "p", UpstreamCurrent: 150, EnabledLocally: false,
	}, 1)
	if out.Confidence != ExternalConfidenceStrongExternal {
		t.Fatalf("want strong_external, got %q", out.Confidence)
	}
	if out.Estimate != 50 {
		t.Fatalf("want estimate 50, got %v", out.Estimate)
	}
}

// TestComputeExternalUsage_MeteringLagClampsToZero: we metered more than upstream has
// posted yet (lag) -> external clamps to 0, verdict CLEAN, never negative.
func TestComputeExternalUsage_MeteringLagClampsToZero(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "p", PeriodStart: 100, PeriodOurCredit: 40}
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "p", UpstreamCurrent: 120, EnabledLocally: true,
	}, 1)
	// upstream delta 20, ours 40 -> raw external -20 -> clamped 0.
	if out.Estimate != 0 {
		t.Fatalf("want estimate clamped to 0, got %v", out.Estimate)
	}
	if out.Confidence != ExternalConfidenceClean {
		t.Fatalf("want clean, got %q", out.Confidence)
	}
}

// TestComputeExternalUsage_PeriodRollover: when the billing period changes we
// re-baseline to the new upstream value, zero the accumulator, and report UNKNOWN
// for that cycle so prior-period usage is never miscounted as external.
func TestComputeExternalUsage_PeriodRollover(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "2026-08-01", PeriodStart: 100, PeriodOurCredit: 80, Estimate: 5}
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "2026-09-01", UpstreamCurrent: 12, EnabledLocally: true,
	}, 999)
	if out.Confidence != ExternalConfidenceUnknown {
		t.Fatalf("want unknown on rollover, got %q", out.Confidence)
	}
	if out.PeriodKey != "2026-09-01" || out.PeriodStart != 12 {
		t.Fatalf("want re-baseline to new period, got %+v", out)
	}
	if out.PeriodOurCredit != 0 || out.Estimate != 0 {
		t.Fatalf("want zeroed accumulator after rollover, got %+v", out)
	}
}

// TestComputeExternalUsage_MissedResetRebaselines: upstream dropped below our
// recorded baseline within the same period key (a reset we didn't observe) ->
// re-baseline and report UNKNOWN rather than a bogus verdict.
func TestComputeExternalUsage_MissedResetRebaselines(t *testing.T) {
	prev := ExternalUsageState{PeriodKey: "p", PeriodStart: 100, PeriodOurCredit: 30}
	out := ComputeExternalUsage(prev, ExternalUsageInput{
		HasUpstream: true, PeriodKey: "p", UpstreamCurrent: 5, EnabledLocally: true,
	}, 1)
	if out.Confidence != ExternalConfidenceUnknown {
		t.Fatalf("want unknown on missed reset, got %q", out.Confidence)
	}
	if out.PeriodStart != 5 || out.PeriodOurCredit != 0 {
		t.Fatalf("want re-baseline, got %+v", out)
	}
}
