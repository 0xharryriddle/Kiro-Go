package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"kiro-go/config"
)

// The pool documents a hard lock-order contract at GetNextForModelExcluding
// (pool/account.go:458-462): read config BEFORE acquiring p.mu, so the pool lock
// never nests cfgLock. Commit 58727ec ("hoist config reads above pool lock to
// avoid freeze under Save stall") hoisted the reads that were violating it.
//
// The reason the contract exists: cfgLock is held for the WHOLE duration of a
// config write, and that write is synchronous file I/O (config.Save ->
// atomicWriteConfig -> os.ReadFile + rotate + CreateTemp + fsync + Rename on a
// ~145KB file). Any pool method that calls config.* while holding p.mu therefore
// parks the pool lock behind disk I/O it does not control. Every other pool
// operation — dispatch, cooldown stamping, model-list updates — queues behind it.
//
// This is a STALL, not a deadlock: package config imports nothing from kiro-go,
// so there is no reverse edge cfgLock -> p.mu. The blast radius is availability
// (the whole pool freezes for the duration of the stalled write), not a hang.
//
// These tests reproduce that mechanism deterministically rather than asserting on
// source text. An earlier version of a sibling test grepped for a guard string
// and produced a false green: the substring still matched when the guard was
// neutralised with `if false && ...`. Source-text assertions cannot distinguish a
// live call site from a dead one, so everything here observes real blocking.

// stallConfigLock parks the cfgLock WRITE lock for the remainder of the test.
//
// The mechanism is config.Init itself: it publishes cfgPath under the lock, then
// calls Load(), which takes cfgLock.Lock() and does os.ReadFile(cfgPath). Opening
// a FIFO for reading blocks until some process opens the write end, so pointing
// cfgPath at a writer-less FIFO holds the write lock open for as long as we like
// — exactly the shape of a slow Save, with none of the timing luck a real Save
// would need.
//
// The returned release func opens the write end with valid JSON so Load finishes
// and the lock is released, then restores a normal on-disk config. Without that
// restore the parked writer would outlive the test and every later test in the
// package that touches config.* would block on it.
func stallConfigLock(t *testing.T) (release func()) {
	t.Helper()

	dir := t.TempDir()
	fifo := filepath.Join(dir, "config.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this platform: %v", err)
	}

	realCfg := filepath.Join(dir, "config.json")

	// Init blocks inside Load's os.ReadFile with cfgLock held.
	initDone := make(chan struct{})
	go func() {
		defer close(initDone)
		_ = config.Init(fifo)
	}()

	// Confirm the lock is actually held before the test proceeds: a plain config
	// read must NOT complete. Asserting this makes the test honest about its own
	// premise instead of assuming the goroutine got there in time.
	//
	// This has to POLL rather than probe once. config.Init takes cfgLock, publishes
	// cfgPath, RELEASES it, and only then calls Load() which re-acquires it — so
	// there is a real (if tiny) window in which a config read legitimately
	// completes. A single probe that lands in that window reports "premise failed"
	// for a reason that has nothing to do with the defect under test; that is
	// exactly what happened on the first run of this file. Once Load owns the lock
	// it never gives it back until the fifo is fed, so retrying until a probe
	// blocks is both sound and terminating.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("premise failed: cfgLock never became held; config reads kept completing, " +
				"so this test cannot observe the stall it is meant to reproduce")
		}
		held := make(chan struct{})
		go func() {
			_ = config.GetAllowOverUsage()
			close(held)
		}()
		select {
		case <-held:
			// Landed in the Init/Load gap (or before Init started). Retry.
			time.Sleep(10 * time.Millisecond)
			continue
		case <-time.After(250 * time.Millisecond):
			// Still blocked: the write lock is held, as intended.
		}
		break
	}

	return func() {
		// Unblock Load by opening the write end and handing it a valid config.
		body, err := json.Marshal(map[string]any{
			"port":     8080,
			"host":     "0.0.0.0",
			"accounts": []any{},
		})
		if err != nil {
			t.Errorf("marshal unblock payload: %v", err)
			return
		}
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open fifo for write: %v", err)
			return
		}
		_, _ = w.Write(body)
		_ = w.Close()

		select {
		case <-initDone:
		case <-time.After(5 * time.Second):
			t.Error("config.Init did not return after the fifo was fed; cfgLock may still be held")
		}

		// Restore a real config file so sibling tests are unaffected.
		if err := config.Init(realCfg); err != nil {
			t.Errorf("restore config: %v", err)
		}
	}
}

// poolFrozenDuringConfigStall reports whether the pool write lock is unobtainable
// while probe (a pool read method) is parked on cfgLock.
//
// SetModelList is the canary: it takes p.mu.Lock() and calls no config function
// at all (pool/account.go:371-382), so if it cannot make progress the cause is
// another pool operation holding p.mu — not any lock of its own.
func poolFrozenDuringConfigStall(t *testing.T, p *AccountPool, probe func()) bool {
	t.Helper()

	probeStarted := make(chan struct{})
	probeDone := make(chan struct{})
	go func() {
		close(probeStarted)
		probe()
		close(probeDone)
	}()
	<-probeStarted

	// Give the probe time to reach its config call site while holding p.mu.
	// If it instead blocks before taking p.mu (the contract-compliant order),
	// this wait is harmless.
	time.Sleep(150 * time.Millisecond)

	frozen := make(chan struct{})
	go func() {
		p.SetModelList("canary", []string{"claude-sonnet-4"})
		close(frozen)
	}()

	select {
	case <-frozen:
		return false
	case <-time.After(1500 * time.Millisecond):
		return true
	}
}

// DiagnosticsFor takes p.mu.RLock and then calls config.GetAllowOverUsage inside
// diagnosticsForLocked (pool/account.go:1425-1427) with no hoisted read of its
// own, so a stalled config write freezes the entire pool behind it.
func TestDiagnosticsDoesNotFreezePoolDuringConfigWrite(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	accounts := []config.Account{
		{ID: "a1", Nickname: "one", Enabled: true},
		{ID: "a2", Nickname: "two", Enabled: true},
	}
	p := &AccountPool{
		accounts:        accounts,
		cooldowns:       make(map[string]time.Time),
		lastDispatchSeq: make(map[string]uint64),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		allowLists:      make(map[string][]string),
	}

	release := stallConfigLock(t)
	defer release()

	if poolFrozenDuringConfigStall(t, p, func() { p.DiagnosticsFor(accounts) }) {
		t.Fatal("pool.DiagnosticsFor held p.mu while blocking on cfgLock: a single slow " +
			"config write froze the whole pool (SetModelList could not acquire p.mu). " +
			"pool/account.go:458-462 requires config reads to be hoisted ABOVE p.mu; " +
			"diagnosticsForLocked calls config.GetAllowOverUsage() under the lock instead.")
	}
}

// ModelRoutingFor reaches the same in-lock config read through the other caller
// of diagnosticsForLocked (pool/account.go:1358-1369). Covering both proves the
// fix belongs in the shared callee rather than in one entry point.
func TestModelRoutingDoesNotFreezePoolDuringConfigWrite(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	accounts := []config.Account{
		{ID: "b1", Nickname: "one", Enabled: true},
	}
	p := &AccountPool{
		accounts:        accounts,
		cooldowns:       make(map[string]time.Time),
		lastDispatchSeq: make(map[string]uint64),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		allowLists:      make(map[string][]string),
	}

	release := stallConfigLock(t)
	defer release()

	if poolFrozenDuringConfigStall(t, p, func() { p.ModelRoutingFor(accounts, "claude-sonnet-4") }) {
		t.Fatal("pool.ModelRoutingFor held p.mu while blocking on cfgLock: a single slow " +
			"config write froze the whole pool. The in-lock config read lives in " +
			"diagnosticsForLocked (pool/account.go:1427) and must be hoisted by both callers.")
	}
}
