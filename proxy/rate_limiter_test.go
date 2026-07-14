package proxy

import "testing"

func TestRateLimiterUnlimitedWhenNoLimits(t *testing.T) {
	rl := newRateLimiter()
	// Both limits 0 -> always allowed, nothing recorded.
	for i := 0; i < 100; i++ {
		if dec := rl.Admit("k", 0, 0, 5, 1000); !dec.Allowed {
			t.Fatalf("expected unconditional admit on iteration %d", i)
		}
	}
}

func TestRateLimiterEmptyKeyAllowed(t *testing.T) {
	rl := newRateLimiter()
	if dec := rl.Admit("", 1, 0, 0, 1000); !dec.Allowed {
		t.Fatalf("expected empty key to be allowed (unauthenticated path)")
	}
}

func TestRateLimiterRpmEnforced(t *testing.T) {
	rl := newRateLimiter()
	now := int64(1000)
	// RPM=3: first 3 admitted, 4th rejected within the same window.
	for i := 0; i < 3; i++ {
		if dec := rl.Admit("k", 3, 0, 0, now); !dec.Allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	dec := rl.Admit("k", 3, 0, 0, now)
	if dec.Allowed {
		t.Fatalf("4th request should be rejected under RPM=3")
	}
	if dec.Reason != "rpm" {
		t.Fatalf("expected rpm reason, got %q", dec.Reason)
	}
	if dec.RetryAfter < 1 {
		t.Fatalf("expected positive RetryAfter, got %d", dec.RetryAfter)
	}
}

func TestRateLimiterWindowSlidesOut(t *testing.T) {
	rl := newRateLimiter()
	// Fill RPM=2 at t=1000.
	rl.Admit("k", 2, 0, 0, 1000)
	rl.Admit("k", 2, 0, 0, 1000)
	if dec := rl.Admit("k", 2, 0, 0, 1000); dec.Allowed {
		t.Fatalf("3rd request in same window should be rejected")
	}
	// After the 60s window fully elapses, the old events prune and admission resumes.
	if dec := rl.Admit("k", 2, 0, 0, 1000+rateWindowSeconds+1); !dec.Allowed {
		t.Fatalf("request after window should be allowed again")
	}
}

func TestRateLimiterTpmRejectsWhenOver(t *testing.T) {
	rl := newRateLimiter()
	now := int64(1000)
	// Admit a request, then fold in 900 tokens against a TPM=1000 budget.
	rl.Admit("k", 0, 1000, 0, now)
	rl.RecordTokens("k", 900, now)
	// Next admission with an estimate that would exceed 1000 is rejected.
	dec := rl.Admit("k", 0, 1000, 200, now)
	if dec.Allowed {
		t.Fatalf("expected TPM rejection when 900+200 > 1000")
	}
	if dec.Reason != "tpm" {
		t.Fatalf("expected tpm reason, got %q", dec.Reason)
	}
}

// TestRateLimiterTpmRejectsExhaustedWindowWithZeroEstimate locks in the real-path
// fix: authenticate() calls Admit with estTokens=0, so an already-exhausted TPM
// window MUST still be rejected. Before the fix the `estTokens > 0` guard let an
// over-budget key keep getting admitted indefinitely.
func TestRateLimiterTpmRejectsExhaustedWindowWithZeroEstimate(t *testing.T) {
	rl := newRateLimiter()
	now := int64(1000)
	// Admit one request, then fold in tokens that meet/exceed the TPM=1000 budget.
	rl.Admit("k", 0, 1000, 0, now)
	rl.RecordTokens("k", 1000, now)
	// Next admission with estTokens=0 (the real authenticate path) must reject.
	dec := rl.Admit("k", 0, 1000, 0, now)
	if dec.Allowed {
		t.Fatalf("expected TPM rejection when window is already at budget, even with estTokens=0")
	}
	if dec.Reason != "tpm" {
		t.Fatalf("expected tpm reason, got %q", dec.Reason)
	}
}

func TestRateLimiterTpmAllowsUnderBudget(t *testing.T) {
	rl := newRateLimiter()
	now := int64(1000)
	rl.Admit("k", 0, 1000, 0, now)
	rl.RecordTokens("k", 500, now)
	if dec := rl.Admit("k", 0, 1000, 200, now); !dec.Allowed {
		t.Fatalf("expected admit when 500+200 <= 1000")
	}
}

func TestRateLimiterRecordTokensNoopWhenNoEvents(t *testing.T) {
	rl := newRateLimiter()
	// No prior Admit -> RecordTokens must not panic or create phantom state.
	rl.RecordTokens("k", 500, 1000)
	// A fresh admit under TPM=1000 should still be allowed (no phantom tokens).
	if dec := rl.Admit("k", 0, 1000, 200, 1000); !dec.Allowed {
		t.Fatalf("expected admit; RecordTokens on empty state should be a no-op")
	}
}
