package proxy

import "testing"

func TestScoreAccountHealth_UnknownBelowMinRequests(t *testing.T) {
	_, tier := scoreAccountHealth(healthInput{TotalRequests: 3, DangerousErrors: 3})
	if tier != HealthTierUnknown {
		t.Fatalf("expected unknown below min requests, got %q", tier)
	}
}

func TestScoreAccountHealth_HealthyWhenNoErrors(t *testing.T) {
	score, tier := scoreAccountHealth(healthInput{TotalRequests: 20})
	if tier != HealthTierHealthy || score != 100 {
		t.Fatalf("expected healthy 100, got %d/%q", score, tier)
	}
}

func TestScoreAccountHealth_CriticalOnHighDangerRate(t *testing.T) {
	// 10/10 dangerous -> -70, plus streak cap -48 -> clamps to 0 -> critical.
	score, tier := scoreAccountHealth(healthInput{TotalRequests: 10, DangerousErrors: 10, ConsecutiveDangerous: 10})
	if tier != HealthTierCritical {
		t.Fatalf("expected critical, got %d/%q", score, tier)
	}
}

func TestScoreAccountHealth_DegradedMidRange(t *testing.T) {
	// 3/20 dangerous = 0.15 rate -> -10.5; no streak. Score ~89 -> healthy.
	// Push into degraded: 6/20 dangerous = 0.30 -> -21, + streak 2 -> -24 => ~55.
	score, tier := scoreAccountHealth(healthInput{TotalRequests: 20, DangerousErrors: 6, ConsecutiveDangerous: 2})
	if tier != HealthTierDegraded {
		t.Fatalf("expected degraded, got %d/%q", score, tier)
	}
}

func TestScoreAccountHealth_MildErrorsDoNotDominate(t *testing.T) {
	// All mild (unknown) errors -> only -15 max -> stays healthy.
	score, tier := scoreAccountHealth(healthInput{TotalRequests: 20, MildErrors: 20})
	if tier != HealthTierHealthy {
		t.Fatalf("expected mild errors to stay healthy, got %d/%q", score, tier)
	}
}

func TestAggregateHealth_ExcludesBenignAndOldEntries(t *testing.T) {
	now := int64(1_000_000)
	window := int64(3600)
	logs := []RequestLog{
		{AccountID: "a", Time: now - 10, Status: "success"},
		{AccountID: "a", Time: now - 20, Status: "error", ErrorType: "quota"},   // benign, excluded
		{AccountID: "a", Time: now - 30, Status: "error", ErrorType: "auth"},    // dangerous
		{AccountID: "a", Time: now - 40, Status: "error", ErrorType: "unknown"}, // mild
		{AccountID: "a", Time: now - 5000, Status: "error", ErrorType: "auth"},  // too old, excluded
		{AccountID: "b", Time: now - 10, Status: "error", ErrorType: "auth"},    // other account
	}
	in := aggregateHealth(logs, "a", now, window)
	if in.TotalRequests != 3 { // success + auth + unknown (quota excluded, old excluded, b excluded)
		t.Fatalf("expected 3 total, got %d", in.TotalRequests)
	}
	if in.DangerousErrors != 1 {
		t.Fatalf("expected 1 dangerous, got %d", in.DangerousErrors)
	}
	if in.MildErrors != 1 {
		t.Fatalf("expected 1 mild, got %d", in.MildErrors)
	}
}

func TestAggregateHealth_ConsecutiveDangerousStreak(t *testing.T) {
	now := int64(1_000_000)
	window := int64(3600)
	// Most recent two are dangerous (auth), preceded by a success. Streak = 2.
	logs := []RequestLog{
		{AccountID: "a", Time: now - 100, Status: "success"},
		{AccountID: "a", Time: now - 50, Status: "error", ErrorType: "auth"},
		{AccountID: "a", Time: now - 10, Status: "error", ErrorType: "suspended"},
	}
	in := aggregateHealth(logs, "a", now, window)
	if in.ConsecutiveDangerous != 2 {
		t.Fatalf("expected streak 2, got %d", in.ConsecutiveDangerous)
	}
}

func TestAggregateHealth_BenignDoesNotBreakStreak(t *testing.T) {
	now := int64(1_000_000)
	window := int64(3600)
	// A quota error between two dangerous ones must be skipped, not break the streak.
	logs := []RequestLog{
		{AccountID: "a", Time: now - 50, Status: "error", ErrorType: "auth"},
		{AccountID: "a", Time: now - 30, Status: "error", ErrorType: "quota"}, // benign, skipped
		{AccountID: "a", Time: now - 10, Status: "error", ErrorType: "auth"},
	}
	in := aggregateHealth(logs, "a", now, window)
	if in.ConsecutiveDangerous != 2 {
		t.Fatalf("expected benign-skipping streak 2, got %d", in.ConsecutiveDangerous)
	}
}

func TestErrorTypeClassification(t *testing.T) {
	for _, et := range []string{"auth", "suspended", "profile"} {
		if !isDangerousErrorType(et) {
			t.Fatalf("expected %q dangerous", et)
		}
	}
	for _, et := range []string{"quota", "overage"} {
		if !isBenignErrorType(et) {
			t.Fatalf("expected %q benign", et)
		}
	}
	if isDangerousErrorType("unknown") || isBenignErrorType("unknown") {
		t.Fatalf("unknown should be neither dangerous nor benign")
	}
}
