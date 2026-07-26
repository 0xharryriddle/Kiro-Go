package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// The cooldown fallback (fallbackEarliestCooldown) is the last resort when no
// healthy candidate exists. It checks exclusion, model support, quota and
// cooldown ordering — but not the circuit breaker, so it hands back an account
// the breaker has explicitly cut off.
//
// Why that matters: RecordError opens the breaker after circuitErrorThreshold
// consecutive failures, and the third error also sets a cooldown. The cooldown
// removes the account from the normal candidate list, which empties the list,
// which routes selection into the fallback — so on a small pool the breaker is
// bypassed by the very condition that opened it, and the failing upstream keeps
// receiving the full request rate.
//
// Liveness is NOT the justification for the bypass: circuitBreaker.isOpen
// promotes open->half-open once circuitOpenDuration elapses and admits a probe
// through the NORMAL candidate path. Refusing here therefore costs at most one
// open window of fast failures instead of a window of guaranteed-failing
// dispatches, and recovery still happens on its own.
func TestCooldownFallbackSkipsOpenCircuit(t *testing.T) {
	p := newTestPool(config.Account{ID: "only", Enabled: true})
	p.circuitState = make(map[string]*circuitBreaker)

	// Drive the real failure path rather than hand-building state, so this test
	// pins the behaviour an operator would actually hit.
	for i := 0; i < circuitErrorThreshold; i++ {
		p.RecordError("only", false)
	}
	if !p.isCircuitOpen("only", time.Now()) {
		t.Fatal("setup: expected the breaker to be open after consecutive errors")
	}

	if got := p.GetNextForModelExcluding("", nil); got != nil {
		t.Fatalf("dispatch returned account %q while its circuit is open", got.ID)
	}
}

// The fallback must still do its job for an account that is merely cooling down
// with a closed breaker — that is the case it exists for (a quota backoff parks
// an account for an hour; refusing to dispatch would take a single-account pool
// dark for that hour). This guards against "fixing" the bypass by disabling the
// fallback outright.
func TestCooldownFallbackStillServesCooldownOnlyAccount(t *testing.T) {
	p := newTestPool(config.Account{ID: "only", Enabled: true})
	p.circuitState = make(map[string]*circuitBreaker)
	p.cooldowns["only"] = time.Now().Add(time.Hour)

	got := p.GetNextForModelExcluding("", nil)
	if got == nil || got.ID != "only" {
		t.Fatalf("cooling-down account with a closed breaker must still be served, got %#v", got)
	}
}

// Once the open window elapses the breaker admits a probe, and that probe must
// arrive through the normal path — proving the fix above cannot wedge a pool
// shut permanently.
func TestOpenCircuitRecoversViaProbeAfterWindow(t *testing.T) {
	p := newTestPool(config.Account{ID: "only", Enabled: true})
	p.circuitState = map[string]*circuitBreaker{
		"only": {
			state:          circuitOpen,
			consecutiveErr: circuitErrorThreshold,
			openedAt:       time.Now().Add(-circuitOpenDuration - time.Second),
		},
	}

	got := p.GetNextForModelExcluding("", nil)
	if got == nil || got.ID != "only" {
		t.Fatalf("expected the half-open probe to be dispatched, got %#v", got)
	}
}
