package config

import "testing"

func TestForecastAccount_NoUpstream(t *testing.T) {
	out := ForecastAccount(AccountForecastInput{HasUpstream: false}, 1000)
	if out.Confidence != ForecastConfidenceUnknown {
		t.Fatalf("expected unknown, got %q", out.Confidence)
	}
}

func TestForecastAccount_NoLimit(t *testing.T) {
	out := ForecastAccount(AccountForecastInput{HasUpstream: true, UsageLimit: 0}, 1000)
	if out.Confidence != ForecastConfidenceUnknown {
		t.Fatalf("expected unknown when no limit, got %q", out.Confidence)
	}
}

func TestForecastAccount_Depleted(t *testing.T) {
	out := ForecastAccount(AccountForecastInput{
		HasUpstream:  true,
		UsageCurrent: 100,
		UsageLimit:   100,
	}, 1000)
	if out.Confidence != ForecastConfidenceDepleted {
		t.Fatalf("expected depleted, got %q", out.Confidence)
	}
	if out.RemainingCredits != 0 {
		t.Fatalf("expected 0 remaining, got %v", out.RemainingCredits)
	}
}

func TestForecastAccount_NoBaselineTimestamp(t *testing.T) {
	out := ForecastAccount(AccountForecastInput{
		HasUpstream:   true,
		UsageCurrent:  20,
		UsageLimit:    100,
		PeriodStart:   0,
		PeriodStartAt: 0, // no baseline captured yet
	}, 100000)
	if out.Confidence != ForecastConfidenceUnknown {
		t.Fatalf("expected unknown without baseline timestamp, got %q", out.Confidence)
	}
	if out.RemainingCredits != 80 {
		t.Fatalf("expected 80 remaining, got %v", out.RemainingCredits)
	}
}

func TestForecastAccount_TooLittleElapsed(t *testing.T) {
	start := int64(1_000_000)
	out := ForecastAccount(AccountForecastInput{
		HasUpstream:   true,
		UsageCurrent:  5,
		UsageLimit:    100,
		PeriodStart:   0,
		PeriodStartAt: start,
	}, start+60) // only 60s elapsed, below the 600s floor
	if out.Confidence != ForecastConfidenceUnknown {
		t.Fatalf("expected unknown with too little elapsed, got %q", out.Confidence)
	}
}

func TestForecastAccount_Idle(t *testing.T) {
	start := int64(1_000_000)
	out := ForecastAccount(AccountForecastInput{
		HasUpstream:   true,
		UsageCurrent:  10,
		UsageLimit:    100,
		PeriodStart:   10, // no consumption since baseline
		PeriodStartAt: start,
	}, start+3600)
	if out.Confidence != ForecastConfidenceIdle {
		t.Fatalf("expected idle when no burn, got %q", out.Confidence)
	}
}

func TestForecastAccount_OK(t *testing.T) {
	start := int64(1_000_000)
	// Consumed 10 credits in 1 hour -> 10 credits/hour. Remaining = 90 -> 9 hours.
	out := ForecastAccount(AccountForecastInput{
		HasUpstream:   true,
		UsageCurrent:  10,
		UsageLimit:    100,
		PeriodStart:   0,
		PeriodStartAt: start,
	}, start+3600)
	if out.Confidence != ForecastConfidenceOK {
		t.Fatalf("expected ok, got %q", out.Confidence)
	}
	if out.BurnPerHour < 9.99 || out.BurnPerHour > 10.01 {
		t.Fatalf("expected ~10 credits/hour, got %v", out.BurnPerHour)
	}
	if out.HoursToExhaust < 8.99 || out.HoursToExhaust > 9.01 {
		t.Fatalf("expected ~9 hours to exhaust, got %v", out.HoursToExhaust)
	}
	expectExhaust := start + 3600 + int64(9*3600)
	if out.ExhaustsAt < expectExhaust-2 || out.ExhaustsAt > expectExhaust+2 {
		t.Fatalf("expected exhaustsAt ~%d, got %d", expectExhaust, out.ExhaustsAt)
	}
}

func TestForecastAccount_RemainingClamped(t *testing.T) {
	start := int64(1_000_000)
	out := ForecastAccount(AccountForecastInput{
		HasUpstream:   true,
		UsageCurrent:  150, // over limit
		UsageLimit:    100,
		PeriodStart:   0,
		PeriodStartAt: start,
	}, start+3600)
	if out.Confidence != ForecastConfidenceDepleted {
		t.Fatalf("expected depleted when over limit, got %q", out.Confidence)
	}
	if out.RemainingCredits != 0 {
		t.Fatalf("expected clamped 0 remaining, got %v", out.RemainingCredits)
	}
}
