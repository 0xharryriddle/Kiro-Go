package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

// isOpen is not a pure query: it PROMOTES open->half-open and stamps probeAt as a
// side effect. A single selection pass now consults the breaker at up to three
// places (quota-aware eligibleForRoute, the LRU candidate loop, the cooldown
// fallback), and every one of them shares the same `now`.
//
// That combination can strand the pool. If an earlier gate promotes the breaker
// and then rejects the account for an unrelated reason (no quota data, so
// quota-aware skips it), the probe has already been spent — and the later gates
// see a half-open breaker whose probe is outstanding and treat the account as
// blocked. The account was ready to be probed, nothing else is routable, and
// selection returns nil: the pool goes dark for a full circuitOpenDuration.
//
// Probe consumption must therefore be idempotent WITHIN one selection pass. All
// call sites in a pass share one `now`, which is exactly the marker needed to
// tell "I already claimed this probe" from "someone else holds it".
func TestOpenCircuitProbeSurvivesMultipleGatesInOnePass(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Quota-aware routing ON, but the account carries NO usable quota data, so
	// eligibleForRoute consults the breaker and pickQuotaAware then discards the
	// account anyway. That is the gate that spends the probe without dispatching.
	if err := config.UpdateQuotaAwareRouting(true); err != nil {
		t.Fatalf("UpdateQuotaAwareRouting: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateQuotaAwareRouting(false) })

	p := newTestPool(config.Account{ID: "only", Enabled: true}) // UsageLimit 0 => no quota data
	p.circuitState = map[string]*circuitBreaker{
		"only": {
			state:          circuitOpen,
			consecutiveErr: circuitErrorThreshold,
			// Open window already elapsed: this account is due a probe.
			openedAt: time.Now().Add(-circuitOpenDuration - time.Second),
		},
	}

	got := p.GetNextForModelExcluding("", nil)
	if got == nil {
		t.Fatal("pool went dark: an account due a half-open probe was rejected because an earlier gate in the same pass consumed the probe")
	}
	if got.ID != "only" {
		t.Fatalf("selected unexpected account %q", got.ID)
	}
}

// The idempotence above must not become a hole: a DIFFERENT selection pass (a
// later request, hence a later `now`) must still be blocked while the first
// pass's probe is outstanding. Otherwise "one probe" degrades back to "every
// request", which is the defect the probe accounting exists to prevent.
func TestHalfOpenProbeStillBlocksLaterPasses(t *testing.T) {
	now := time.Now()
	cb := &circuitBreaker{
		state:          circuitOpen,
		consecutiveErr: circuitErrorThreshold,
		openedAt:       now.Add(-circuitOpenDuration - time.Second),
	}

	// isOpen is a pure predicate, so every gate in pass 1 agrees: the account
	// is admissible because a probe is due.
	for i := 0; i < 3; i++ {
		if cb.isOpen(now) {
			t.Fatalf("gate %d of the same pass must see the account as admissible", i+1)
		}
	}

	// Dispatch commits: the selected account's probe is claimed exactly once.
	cb.claimProbe(now)
	if cb.state != circuitHalfOpen {
		t.Fatalf("claimProbe must persist the half-open transition, got state %d", cb.state)
	}

	// A later pass, while that probe is still outstanding, must be blocked —
	// otherwise "one probe" degrades back to "every request".
	if !cb.isOpen(now.Add(time.Second)) {
		t.Fatal("a later pass must be blocked while the first pass's probe is outstanding")
	}

	// And once the probe ages out unanswered, a fresh probe is allowed.
	if cb.isOpen(now.Add(circuitOpenDuration + 2*time.Second)) {
		t.Fatal("a fresh probe must be admitted once the outstanding one aged out")
	}
}
