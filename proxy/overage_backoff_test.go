package proxy

import (
	"errors"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// An upstream 402 overage means "this account has hit its paid-overage ceiling".
// The account is not broken, but it CANNOT serve the next request either — so it
// must be parked long enough for selection to move on.
//
// The live path does not park it. `handleAccountFailure`'s overage branch
// (proxy/account_failover.go:453-455) calls `disableAccountOverage` — which only
// re-reads the upstream Overages switch and returns EARLY if that read fails —
// followed by `RecordError(id, false)`. The `false` routes to the transient
// branch of `RecordError` (pool/account.go:881-887), which needs THREE
// consecutive errors before it applies even a 1-minute cooldown. So the first
// two 402s leave no backoff at all and the same exhausted account is immediately
// re-selectable.
//
// `MarkOverLimit` (pool/account.go:1194) exists precisely for this and applies a
// 1-hour backoff via `setCooldownIfLater`. It has ZERO non-test callers. The
// repo's own design spec says the mapping should be `402 -> pool.MarkOverLimit`
// (docs/superpowers/specs/2026-06-28-auth-upstream-reliability-design.md:83).
//
// Live evidence from data/traces/index-20260727.jsonl: of 21 `HTTP 402 overage`
// attempts on one account, 20 were followed by another attempt on that SAME
// account within 60 seconds, median gap 0s.
//
// These tests observe re-selection through the real selector rather than
// asserting on source text: a source-text assertion cannot tell a live call from
// a dead one (see the round-14 false green recorded in the checkpoint).

// overageErrLive is the exact error form the live proxy produces for a 402.
// `upstreamError` (proxy/kiro.go:426-428) injects the word "overage" so
// `isOverageErrorMessage` (which requires BOTH a 402 digit-boundary token AND
// the word) matches. Using the real string keeps the test honest about which
// branch it exercises.
const overageErrLive = `HTTP 402 overage from Kiro IDE: {"message":"You have reached the limit"}`

// selectedIDs dispatches n times and reports which accounts the selector handed
// back. Cooldowns are the mechanism under test, so this deliberately goes through
// GetNextForModelExcluding rather than inspecting the cooldown map directly.
func selectedIDs(p *accountpool.AccountPool, n int) map[string]int {
	out := make(map[string]int)
	for i := 0; i < n; i++ {
		acc := p.GetNextForModelExcluding("", nil)
		if acc == nil {
			out["<nil>"]++
			continue
		}
		out[acc.ID]++
	}
	return out
}

// The load-bearing behaviour: after a 402 overage the exhausted account must not
// be handed straight back while a healthy alternative exists.
func TestOverageErrorParksAccountSoSelectionMovesOn(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Two accounts: the one that 402s, and a healthy alternative. Without an
	// alternative "no cooldown" and "correctly kept as last resort" would be
	// indistinguishable.
	for _, a := range []config.Account{
		{ID: "hot", Enabled: true, Email: "hot@example.test"},
		{ID: "spare", Enabled: true, Email: "spare@example.test"},
	} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}
	hot, ok := config.GetAccountByID("hot")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	h.handleAccountFailure(&hot, errors.New(overageErrLive))

	// One dispatch is enough to show the defect: with no backoff the LRU clock
	// still favours "hot" (it has not been stamped as dispatched), so it comes
	// straight back.
	got := selectedIDs(p, 4)
	if got["hot"] > 0 {
		t.Fatalf("a 402-overage account was re-selected %d/4 times immediately after the overage "+
			"(selections: %v). handleAccountFailure's overage branch applies no durable backoff: "+
			"disableAccountOverage only re-reads upstream status (and returns early if that read "+
			"fails), and RecordError(id, false) needs 3 consecutive errors before even a 1-minute "+
			"cooldown. pool.MarkOverLimit applies the 1h backoff this needs and has no callers.",
			got["hot"], got)
	}
}

// The overage account must not be permanently banned either — overage clears when
// the upstream period rolls over, so this is a backoff, not a disable. Guards
// against "fixing" the above by disabling the account.
func TestOverageErrorDoesNotBanTheAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "hot2", Enabled: true, Email: "hot2@example.test"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	hot, ok := config.GetAccountByID("hot2")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	h.handleAccountFailure(&hot, errors.New(overageErrLive))

	after, _ := config.GetAccountByID("hot2")
	if !after.Enabled || after.BanStatus == "BANNED" {
		t.Fatalf("402 overage permanently disabled the account: enabled=%v banStatus=%q. "+
			"Overage clears at the next upstream period roll-over, so this must be a timed "+
			"backoff, not a ban.", after.Enabled, after.BanStatus)
	}
}

// A single account pool must still be reachable eventually rather than going
// permanently dark: the cooldown fallback (fallbackEarliestCooldown) is what
// keeps a one-account deployment alive. This pins that the backoff is a cooldown
// the fallback can see, not an exclusion.
func TestOverageBackoffStillAllowsSingleAccountFallback(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "only", Enabled: true, Email: "only@example.test"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	only, ok := config.GetAccountByID("only")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	h.handleAccountFailure(&only, errors.New(overageErrLive))

	if acc := p.GetNextForModelExcluding("", nil); acc == nil {
		t.Fatal("a one-account pool went fully dark after a 402 overage; the earliest-cooldown " +
			"fallback must still surface the only account rather than returning nil")
	}
}

// Contrast case, so the fix cannot be "cool everything down for an hour": a
// generic transient failure must keep its short 3-strike behaviour.
func TestGenericFailureDoesNotGetTheOverageBackoff(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, a := range []config.Account{
		{ID: "g1", Enabled: true, Email: "g1@example.test"},
		{ID: "g2", Enabled: true, Email: "g2@example.test"},
	} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}
	g1, ok := config.GetAccountByID("g1")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	// One generic 502: below the 3-error threshold, so no cooldown yet and the
	// account stays selectable.
	h.handleAccountFailure(&g1, errors.New("HTTP 502 from Kiro IDE: bad gateway"))

	got := selectedIDs(p, 6)
	if got["g1"] == 0 {
		t.Fatalf("a single generic failure parked the account (selections: %v); only overage/quota "+
			"failures carry a long backoff, transient ones need 3 strikes for 1 minute", got)
	}
}

// Pins the classifier boundary the fix depends on: the live 402 form must reach
// the overage branch, and a 402 WITHOUT the injected marker must not be mistaken
// for one. 229 untagged `HTTP 402 from Kiro IDE:` attempts exist in the corpus
// from 2026-07-25, before upstreamError began injecting the word.
func TestOverageClassifierMatchesTheLiveErrorForm(t *testing.T) {
	if !isOverageErrorMessage(overageErrLive) {
		t.Fatalf("the live 402 error form is not classified as overage: %q", overageErrLive)
	}
	if isOverageErrorMessage("HTTP 402 from Kiro IDE: {\"message\":\"You have reached the limit\"}") {
		t.Fatal("an untagged 402 was classified as overage; isOverageErrorMessage requires the " +
			"injected 'overage' marker and must not match on the status token alone")
	}
	if isOverageErrorMessage("HTTP 429 from Kiro IDE: quota exhausted") {
		t.Fatal("a 429 quota error was misclassified as overage")
	}
}

// Guards the invariant the fix must preserve: an overage backoff must never be
// shortened by a later transient failure on the same account.
func TestOverageBackoffSurvivesALaterTransientFailure(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, a := range []config.Account{
		{ID: "s1", Enabled: true, Email: "s1@example.test"},
		{ID: "s2", Enabled: true, Email: "s2@example.test"},
	} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}
	s1, ok := config.GetAccountByID("s1")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	h.handleAccountFailure(&s1, errors.New(overageErrLive))
	// Three transient failures would normally stamp a 1-minute cooldown; that
	// must not replace a longer overage backoff.
	for i := 0; i < 3; i++ {
		h.handleAccountFailure(&s1, errors.New("HTTP 502 from Kiro IDE: bad gateway"))
	}

	var until time.Time
	for _, d := range p.Diagnostics() {
		if d.ID == "s1" {
			until = time.Unix(d.CooldownUntil, 0)
		}
	}
	if remaining := time.Until(until); remaining < 10*time.Minute {
		t.Fatalf("the overage backoff was shortened to %v by later transient failures; "+
			"setCooldownIfLater must keep the longer one", remaining)
	}
}
