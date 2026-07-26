package pool

import (
	"kiro-go/config"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The circuit breaker's open->half-open transition is documented as "allow one
// probe" (see isOpen). A half-open breaker that returns "not open" to every
// caller is not a probe, it is a fully re-opened gate: the whole point of
// half-open is to send ONE request at the suspect account and decide from its
// outcome. Without a single-probe rule, the instant the 30s window elapses the
// full request rate resumes against an account that just failed five times in a
// row, which is the hammering the breaker exists to prevent.
func TestHalfOpenCircuitAdmitsOneProbeAtATime(t *testing.T) {
	now := time.Now()
	cb := &circuitBreaker{
		state:          circuitOpen,
		consecutiveErr: circuitErrorThreshold,
		openedAt:       now.Add(-circuitOpenDuration - time.Second),
	}

	// First caller after the window is admissible, and claiming the probe at
	// its dispatch commit point persists the half-open transition.
	if cb.isOpen(now) {
		t.Fatal("expected the first caller after the open window to be admitted as a probe")
	}
	cb.claimProbe(now)
	if cb.state != circuitHalfOpen {
		t.Fatalf("expected half-open after the probe was claimed, got state %d", cb.state)
	}

	// Every subsequent caller must be blocked while that probe is outstanding.
	// (A later instant, because a repeated check at the same instant is the same
	// selection pass re-asking — see TestOpenCircuitProbeSurvivesMultipleGatesInOnePass.)
	for i := 0; i < 3; i++ {
		if !cb.isOpen(now.Add(time.Duration(i+1) * time.Second)) {
			t.Fatalf("half-open breaker admitted a second concurrent probe (attempt %d)", i+2)
		}
	}
}

// A probe that never reports back (dropped request, process restart mid-flight)
// must not wedge the breaker shut forever: after another open window elapses a
// fresh probe is allowed. This guards the availability side of the fix above.
func TestHalfOpenCircuitAllowsFreshProbeAfterWindow(t *testing.T) {
	now := time.Now()
	cb := &circuitBreaker{
		state:          circuitOpen,
		consecutiveErr: circuitErrorThreshold,
		openedAt:       now.Add(-circuitOpenDuration - time.Second),
	}
	if cb.isOpen(now) {
		t.Fatal("expected the first probe to be admitted")
	}
	cb.claimProbe(now) // the first probe is dispatched
	if !cb.isOpen(now.Add(time.Second)) {
		t.Fatal("expected a later caller to be blocked while the probe is outstanding")
	}
	// The probe never reported back (no recordError, no reset): once another
	// open window elapses, a fresh probe must be admitted rather than the
	// breaker staying wedged shut forever.
	later := now.Add(circuitOpenDuration + time.Second)
	if cb.isOpen(later) {
		t.Fatal("expected a fresh probe once the outstanding one aged out")
	}
}

// Quota-aware routing (F2) is a second, independent selection path. The LRU path
// skips accounts whose circuit is open; the quota-aware path went through
// eligibleForRoute, which checked excluded/cooldown/quota but NOT the breaker.
// The account with the most remaining quota is exactly the one a burst of
// failures leaves untouched, so enabling F2 preferentially routed traffic at the
// account the breaker had just cut off.
func TestQuotaAwareRoutingSkipsOpenCircuit(t *testing.T) {
	enableQuotaAwareForTest(t)

	p := healthPool(
		config.Account{ID: "broken", Enabled: true, UsageLimit: 100, UsageCurrent: 0},
		config.Account{ID: "healthy", Enabled: true, UsageLimit: 100, UsageCurrent: 90},
	)
	p.circuitState["broken"] = &circuitBreaker{
		state:          circuitOpen,
		consecutiveErr: circuitErrorThreshold,
		openedAt:       time.Now(),
	}

	got := p.GetNextForModelExcluding("", nil)
	if got == nil {
		t.Fatal("expected the healthy account to be selected")
	}
	if got.ID == "broken" {
		t.Fatal("quota-aware routing selected an account whose circuit is open")
	}
}

// Every dispatch path must stamp the monotonic LRU clock. The code states this
// invariant on the cooldown fallback ("every other dispatch path stamps; the
// fallback must too, or a degraded account's stale seq wins the next pick and
// gets a burst"), and the quota-aware path violated it. The consequence shows up
// on toggle: an account that served all traffic under quota-aware routing still
// looks never-dispatched to LRU, so turning F2 off hands it another burst.
func TestQuotaAwareDispatchStampsLruClock(t *testing.T) {
	enableQuotaAwareForTest(t)

	p := healthPool(
		config.Account{ID: "most-quota", Enabled: true, UsageLimit: 100, UsageCurrent: 0},
		config.Account{ID: "less-quota", Enabled: true, UsageLimit: 100, UsageCurrent: 90},
	)

	got := p.GetNextForModelExcluding("", nil)
	if got == nil || got.ID != "most-quota" {
		t.Fatalf("expected the most-remaining-quota account, got %#v", got)
	}
	if p.lastDispatchSeq["most-quota"] == 0 {
		t.Fatal("quota-aware dispatch did not stamp the LRU clock for the account it selected")
	}
}

// Per-account state maps are keyed by account ID and were never pruned, so every
// deleted account left its circuit breaker, LRU stamp, health stats and cooldown
// behind for the process lifetime. On a long-lived proxy whose fleet is rotated
// this is an unbounded leak, and it also means state can outlive the account it
// describes.
func TestReloadPrunesStateForDeletedAccounts(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "gone", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "stays", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.lastDispatchSeq = make(map[string]uint64)
	p.Reload()

	p.circuitState["gone"] = &circuitBreaker{state: circuitOpen, openedAt: time.Now()}
	p.lastDispatchSeq["gone"] = 123
	p.cooldowns["gone"] = time.Now().Add(time.Hour)
	p.healthStats["gone"] = &accountHealth{samples: 3}
	p.apiKeyAffinity["key-for-gone"] = apiKeyBinding{accountID: "gone", lastUsed: time.Now()}

	p.circuitState["stays"] = &circuitBreaker{state: circuitClosed}
	p.lastDispatchSeq["stays"] = 7

	if err := config.DeleteAccount("gone"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	p.Reload()

	if _, ok := p.circuitState["gone"]; ok {
		t.Error("Reload kept the circuit breaker of a deleted account")
	}
	if _, ok := p.lastDispatchSeq["gone"]; ok {
		t.Error("Reload kept the LRU stamp of a deleted account")
	}
	if _, ok := p.cooldowns["gone"]; ok {
		t.Error("Reload kept the cooldown of a deleted account")
	}
	if _, ok := p.healthStats["gone"]; ok {
		t.Error("Reload kept the health stats of a deleted account")
	}
	if _, ok := p.apiKeyAffinity["key-for-gone"]; ok {
		t.Error("Reload kept a session-affinity binding pointing at a deleted account")
	}

	// A surviving account must keep everything: pruning is by absence from
	// config, never by absence from the routable set.
	if _, ok := p.circuitState["stays"]; !ok {
		t.Error("Reload dropped the circuit breaker of a live account")
	}
	if p.lastDispatchSeq["stays"] != 7 {
		t.Error("Reload dropped the LRU stamp of a live account")
	}
}

// A merely-disabled or quota-blocked account is absent from the routable pool but
// still present in config. Its cooldown and breaker MUST survive a Reload: a
// quota cooldown is an hour long and dropping it would let the exhausted upstream
// be re-selected the moment it is re-enabled.
func TestReloadKeepsStateForDisabledAccount(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "paused", Enabled: false, BanStatus: "DISABLED"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool()
	p.lastDispatchSeq = make(map[string]uint64)
	p.Reload()
	p.cooldowns["paused"] = time.Now().Add(time.Hour)
	p.circuitState["paused"] = &circuitBreaker{state: circuitOpen, openedAt: time.Now()}

	p.Reload()

	if _, ok := p.cooldowns["paused"]; !ok {
		t.Error("Reload dropped the cooldown of a disabled (but still configured) account")
	}
	if _, ok := p.circuitState["paused"]; !ok {
		t.Error("Reload dropped the breaker of a disabled (but still configured) account")
	}
}

// maxAffinityEntries is documented as a bound on the session-affinity map, but
// the only eviction pass deleted TTL-expired bindings. Under steady traffic from
// many distinct keys no binding is expired, so the map grew without limit — one
// live entry per API key ever seen.
func TestAffinityMapEnforcesHardBound(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("SetSessionAffinityEnabled: %v", err)
	}
	t.Cleanup(func() { _ = config.SetSessionAffinityEnabled(false) })
	if err := config.AddAccount(config.Account{ID: "a", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := healthPool(config.Account{ID: "a", Enabled: true})
	for i := 0; i < maxAffinityEntries*2; i++ {
		p.GetNextForModelWithApiKey("", nil, "fresh-key-"+strconv.Itoa(i))
	}
	if len(p.apiKeyAffinity) > maxAffinityEntries {
		t.Fatalf("affinity map grew to %d entries despite a documented bound of %d",
			len(p.apiKeyAffinity), maxAffinityEntries)
	}
}
