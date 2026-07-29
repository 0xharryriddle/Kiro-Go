package proxy

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Round 17 / A2. Before Handler.Close existed, nothing ever closed stopRefresh or
// stopStatsSaver: both channels had readers (backgroundRefresh + importWatchLoop
// on the former, backgroundStatsSaver + backgroundTracePrune on the latter) and no
// writer at all. The loops could only die with the process, and main.go had no
// signal handling, so SIGTERM dropped pending flushes on the floor.
//
// These tests pin the contract rather than the implementation: a select on the
// stop channel must actually observe a close, Close must be idempotent, and it
// must survive the bare `&Handler{}` literals the suite builds 169 times.

// TestCloseStopsRefreshLoopSelectors proves a goroutine selecting on stopRefresh
// — the real shape of backgroundRefresh and importWatchLoop — is released by
// Close. Neutralizing the `close(h.stopRefresh)` in Close hangs this test until
// the deadline, which is exactly the pre-fix behaviour.
func TestCloseStopsRefreshLoopSelectors(t *testing.T) {
	h := &Handler{
		stopRefresh:    make(chan struct{}),
		stopStatsSaver: make(chan struct{}),
	}

	released := make(chan struct{})
	go func() {
		// Mirrors backgroundRefresh's select arm.
		<-h.stopRefresh
		close(released)
	}()

	h.Close()

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release a goroutine waiting on stopRefresh: " +
			"background refresh / import-watch loops would outlive shutdown")
	}
}

// TestCloseStopsStatsSaverSelectors is the same contract for the other channel,
// which two independent loops depend on (stats saver and trace prune).
func TestCloseStopsStatsSaverSelectors(t *testing.T) {
	h := &Handler{
		stopRefresh:    make(chan struct{}),
		stopStatsSaver: make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// Two waiters, because a `close` (not a send) is what makes BOTH loops exit.
	// A naive `h.stopStatsSaver <- struct{}{}` implementation would release only
	// one and this test would catch it.
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-h.stopStatsSaver
		}()
	}

	h.Close()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release all goroutines waiting on stopStatsSaver: " +
			"a send instead of a close would only wake one loop")
	}
}

// TestCloseIsIdempotent guards the panic path. `close` on an already-closed
// channel panics, so a Close without a once-guard would take the process down
// when both a signal handler and a deferred cleanup call it — the exact hazard
// promptCacheTracker.Stop already documents for itself.
func TestCloseIsIdempotent(t *testing.T) {
	h := &Handler{
		stopRefresh:    make(chan struct{}),
		stopStatsSaver: make(chan struct{}),
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Close panicked (close of closed channel): %v", r)
		}
	}()

	h.Close()
	h.Close()
	h.Close()
}

// TestCloseSurvivesBareHandlerLiteral is the compatibility control. The suite
// builds `&Handler{pool: p}` in 169 places, leaving stop channels nil and caches
// nil. Closing a nil channel panics and promptCacheTracker.Stop dereferences its
// receiver, so Close must filter both.
func TestCloseSurvivesBareHandlerLiteral(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Close panicked on a bare &Handler{} literal: %v", r)
		}
	}()

	h := &Handler{} // nil channels, nil promptCache, nil traceStore
	h.Close()
	h.Close()
}

// TestCloseOnNilHandlerIsSafe covers the nil-receiver guard.
func TestCloseOnNilHandlerIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Close panicked on a nil *Handler: %v", r)
		}
	}()

	var h *Handler
	h.Close()
}

// TestCloseFlushesPromptCache proves Close persists state rather than merely
// stopping timers. The tracker is dirtied, then Close must land it on disk —
// this is the data-loss half of the defect, distinct from the goroutine-leak
// half above.
func TestCloseFlushesPromptCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt_cache.json")

	tr := newPromptCacheTracker(time.Hour)
	// A long flush interval guarantees the periodic ticker cannot be what writes
	// the file — only the final flush inside Stop can.
	tr.startSaveLoop(path, time.Hour)

	// Dirty the tracker through its real internals. There is no exported Put;
	// putLocked expects the caller to hold t.mu, and flush() is a no-op unless
	// dirty is set — so both are done explicitly here rather than relying on a
	// higher-level path (Update) that would drag ClaudeRequest plumbing in.
	var fp [32]byte
	copy(fp[:], "fingerprint-close-test")
	tr.mu.Lock()
	tr.putLocked(fp, time.Now().Add(time.Hour), time.Hour)
	tr.dirty = true
	tr.mu.Unlock()

	h := &Handler{
		stopRefresh:    make(chan struct{}),
		stopStatsSaver: make(chan struct{}),
		promptCache:    tr,
	}
	h.Close()

	// Stop's final flush is synchronous inside the loop goroutine, so allow it a
	// moment to land rather than asserting on a race.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return // flushed
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Close did not flush the prompt cache to %s: dirty cache state is lost on shutdown", path)
}
