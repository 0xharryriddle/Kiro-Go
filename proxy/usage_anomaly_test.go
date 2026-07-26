package proxy

import "testing"

func TestDetectUsageAnomaly_UnknownBelowMinBaseline(t *testing.T) {
	// Fewer than anomalyMinBaselineRequests baseline requests -> no verdict.
	tier, _ := detectUsageAnomaly(anomalyInput{
		RecentRequests:    5,
		BaselineRequests:  3,
		RecentRatePerHr:   5,
		BaselineRatePerHr: 0.125,
	})
	if tier != AnomalyTierUnknown {
		t.Fatalf("expected unknown below min baseline, got %q", tier)
	}
}

func TestDetectUsageAnomaly_NormalWithinTolerance(t *testing.T) {
	// Recent rate ~= baseline rate -> normal.
	tier, ratio := detectUsageAnomaly(anomalyInput{
		RecentRequests:    10,
		BaselineRequests:  240,
		RecentRatePerHr:   10,
		BaselineRatePerHr: 10,
	})
	if tier != AnomalyTierNormal {
		t.Fatalf("expected normal, got %q (ratio=%v)", tier, ratio)
	}
	if ratio != 1.0 {
		t.Fatalf("expected ratio 1.0, got %v", ratio)
	}
}

func TestDetectUsageAnomaly_SpikeWhenRateExceedsMultiplier(t *testing.T) {
	// Recent hourly rate 5x baseline, with real recent volume -> spike.
	tier, ratio := detectUsageAnomaly(anomalyInput{
		RecentRequests:    50,
		BaselineRequests:  100,
		RecentRatePerHr:   50,
		BaselineRatePerHr: 10,
	})
	if tier != AnomalyTierSpike {
		t.Fatalf("expected spike, got %q (ratio=%v)", tier, ratio)
	}
	if ratio != 5.0 {
		t.Fatalf("expected ratio 5.0, got %v", ratio)
	}
}

func TestDetectUsageAnomaly_NoSpikeOnStrayRequestsOverIdleBaseline(t *testing.T) {
	// High ratio but recent volume below anomalyMinRecentRequests -> not a spike.
	tier, _ := detectUsageAnomaly(anomalyInput{
		RecentRequests:    2,
		BaselineRequests:  20,
		RecentRatePerHr:   2,
		BaselineRatePerHr: 0.1,
	})
	if tier != AnomalyTierNormal {
		t.Fatalf("expected normal (too few recent requests), got %q", tier)
	}
}

func TestAggregateAnomaly_CountsRecentAndBaselineWindows(t *testing.T) {
	now := int64(1_000_000)
	logs := []RequestLog{
		{AccountID: "a", Time: now - 100},     // recent
		{AccountID: "a", Time: now - 200},     // recent
		{AccountID: "a", Time: now - 2*3600},  // baseline, not recent
		{AccountID: "a", Time: now - 25*3600}, // outside baseline
		{AccountID: "b", Time: now - 100},     // other account
	}
	in := aggregateAnomaly(logs, "a", now)
	if in.RecentRequests != 2 {
		t.Fatalf("expected 2 recent, got %d", in.RecentRequests)
	}
	if in.BaselineRequests != 3 {
		t.Fatalf("expected 3 baseline (within 24h), got %d", in.BaselineRequests)
	}
	// Recent rate = 2 requests / 1h = 2/hr.
	if in.RecentRatePerHr != 2.0 {
		t.Fatalf("expected recent rate 2/hr, got %v", in.RecentRatePerHr)
	}
	// Baseline rate = 3 requests / 24h = 0.125/hr.
	if in.BaselineRatePerHr != 0.125 {
		t.Fatalf("expected baseline rate 0.125/hr, got %v", in.BaselineRatePerHr)
	}
}
