package proxy

// Per-API-key windowed rate limiting (F6): sliding-window RPM (requests/60s) and
// TPM (tokens/60s) counters enforced in-process, distinct from the cumulative
// token/credit limits in config (which never reset).
//
// HONESTY NOTE: counters are in-memory only. In a multi-instance deployment each
// instance enforces its own window, so the effective fleet limit is N * limit.
// This matches the rest of the proxy's single-instance assumption (no Redis).
//
// Window model: a simple fixed-lag sliding window. Each key tracks timestamped
// events (request markers carry tokens=0-with-a-count, token events carry the
// token delta). On each check we drop events older than the window, sum what
// remains, and admit only if adding the new request keeps us within the limit.

import "sync"

// rateWindowSeconds is the rolling window width for RPM/TPM enforcement.
const rateWindowSeconds int64 = 60

// rateEvent is one recorded request within the window.
type rateEvent struct {
	at     int64 // Unix seconds
	tokens int64 // tokens attributed to this request (0 at admission, updated post-response is out of scope)
}

// keyRateState holds the sliding-window events for one API key.
type keyRateState struct {
	events []rateEvent
}

// rateLimiter is the in-process per-key limiter. Safe for concurrent use.
type rateLimiter struct {
	mu     sync.Mutex
	states map[string]*keyRateState
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{states: make(map[string]*keyRateState)}
}

// pruneLocked drops events outside the window [now-rateWindowSeconds, now].
func (s *keyRateState) pruneLocked(now int64) {
	cutoff := now - rateWindowSeconds
	i := 0
	for ; i < len(s.events); i++ {
		if s.events[i].at >= cutoff {
			break
		}
	}
	if i > 0 {
		s.events = append([]rateEvent(nil), s.events[i:]...)
	}
}

// currentLocked returns the request count and token sum within the window.
func (s *keyRateState) currentLocked() (reqs int64, tokens int64) {
	for _, e := range s.events {
		reqs++
		tokens += e.tokens
	}
	return
}

// rateDecision is the outcome of an admission check.
type rateDecision struct {
	Allowed    bool
	Reason     string // "rpm" | "tpm" | "" when allowed
	RetryAfter int64  // seconds until the oldest in-window event expires
}

// Admit checks whether a new request for keyID is allowed under the given RPM/TPM
// limits (0 = unlimited). estTokens is the pre-request token estimate charged to
// the TPM window. When allowed, the request is recorded. `now` is injectable for
// tests. When both limits are 0, admission is unconditional and nothing is stored.
func (rl *rateLimiter) Admit(keyID string, rpmLimit, tpmLimit, estTokens, now int64) rateDecision {
	if keyID == "" || (rpmLimit <= 0 && tpmLimit <= 0) {
		return rateDecision{Allowed: true}
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st := rl.states[keyID]
	if st == nil {
		st = &keyRateState{}
		rl.states[keyID] = st
	}
	st.pruneLocked(now)
	reqs, tokens := st.currentLocked()

	retryAfter := func() int64 {
		if len(st.events) == 0 {
			return rateWindowSeconds
		}
		ra := st.events[0].at + rateWindowSeconds - now
		if ra < 1 {
			ra = 1
		}
		return ra
	}

	if rpmLimit > 0 && reqs+1 > rpmLimit {
		return rateDecision{Allowed: false, Reason: "rpm", RetryAfter: retryAfter()}
	}
	if tpmLimit > 0 {
		// Two independent TPM rejections:
		//  1. The window is ALREADY at/over budget (actual tokens folded back via
		//     RecordTokens after prior responses). This must fire even when the
		//     admission estimate is 0 — otherwise an exhausted key keeps getting
		//     admitted, which was the real-path bypass (authenticate passes
		//     estTokens=0, so the old `estTokens > 0` guard never rejected).
		//  2. This request's estimate would push the window over budget.
		if tokens >= tpmLimit || (estTokens > 0 && tokens+estTokens > tpmLimit) {
			return rateDecision{Allowed: false, Reason: "tpm", RetryAfter: retryAfter()}
		}
	}

	st.events = append(st.events, rateEvent{at: now, tokens: estTokens})
	return rateDecision{Allowed: true}
}

// RecordTokens attributes actual token usage to a key's most recent window event
// after a response completes. This backs the best-effort TPM model: admission
// records the request marker (0 tokens), and the real token count is folded in
// here, so the NEXT request's TPM check sees recent consumption. No-op when the
// key has no recorded events (e.g. limits are unset). `now` is injectable.
func (rl *rateLimiter) RecordTokens(keyID string, tokens, now int64) {
	if keyID == "" || tokens <= 0 {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	st := rl.states[keyID]
	if st == nil || len(st.events) == 0 {
		return
	}
	st.pruneLocked(now)
	if len(st.events) == 0 {
		return
	}
	// Attribute to the most recent event (the request that produced these tokens).
	st.events[len(st.events)-1].tokens += tokens
}
