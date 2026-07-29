package proxy

import (
	"sync"

	"kiro-go/logger"
)

// closeState carries the once-guard for Handler.Close. It lives in its own
// struct-embedded field rather than as a bare sync.Once on Handler so the 169
// `&Handler{...}` literals in the test suite keep compiling untouched — a zero
// sync.Once is already usable, so no test needs to initialise anything.
type closeState struct {
	once sync.Once
}

// Close stops every background goroutine the Handler owns and flushes the state
// they are responsible for persisting. It is safe to call on a partially
// constructed Handler (the test suite builds `&Handler{pool: p}` literals with
// nil caches and nil stop channels) and safe to call more than once.
//
// Why this exists: before round 17 nothing ever closed stopRefresh or
// stopStatsSaver. Both channels had readers — backgroundRefresh and
// importWatchLoop select on stopRefresh; backgroundStatsSaver and
// backgroundTracePrune select on stopStatsSaver — but no writer, so the loops
// could only ever die with the process. Combined with main.go having no signal
// handling at all, SIGTERM killed the process mid-flush: pending stats, the
// prompt cache, and queued trace rows were simply lost.
//
// Ordering is deliberate:
//  1. Signal the ticker loops first, so nothing schedules new work while we drain.
//  2. saveStats() explicitly, rather than relying on backgroundStatsSaver's own
//     exit-path save. That loop does save on stop, but Close does not wait for
//     it, so on a fast process exit the save could lose the race. Calling it here
//     makes persistence ordered with respect to Close returning. A double save is
//     harmless — UpdateStats just writes the current atomic counters again.
//  3. promptCache.Stop() and traceStore.Close(), which each perform a final flush
//     and (for traceStore) block until the writer goroutine has drained its queue.
func (h *Handler) Close() {
	if h == nil {
		return
	}
	h.closeState.once.Do(func() {
		// Nil checks throughout: tests construct Handlers directly without the
		// channels NewHandler would have made. Closing a nil channel panics.
		if h.stopRefresh != nil {
			close(h.stopRefresh)
		}
		if h.stopStatsSaver != nil {
			close(h.stopStatsSaver)
		}

		// Only meaningful when this Handler was tracking stats at all; a bare
		// test literal has zeroed counters and no config wiring to write to.
		h.saveStats()

		// promptCacheTracker.Stop dereferences its receiver (stopOnce.Do), so a
		// nil tracker must be filtered here rather than inside Stop.
		if h.promptCache != nil {
			h.promptCache.Stop()
		}

		// traceStore.Close is already nil-receiver safe and waits for its writer
		// goroutine, so queued trace rows reach disk before we return.
		h.traceStore.Close()

		logger.Infof("Handler closed: background loops stopped, state flushed")
	})
}
