package proxy

// Brute-force resistance for the two admin authentication paths (A4).
//
// Both admin gates compare a shared secret in constant time — handleAdminAPI
// (handler.go) for /admin/api/*, and authenticateAdminKey (admin_bot_api.go) for
// the nine machine-integration routes — but neither counted failures. A caller
// who could reach the port could guess the admin password without limit, and a
// correct guess yields /admin/api/config/export: the raw config.json, including
// refresh tokens and ksk_ keys. Constant-time comparison closes a timing oracle;
// it does nothing about volume.
//
// This file adds the missing half: a per-source-IP failure counter with
// exponential lockout, shared by BOTH gates.

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// adminAuthFailureThreshold is how many failures a source IP may accumulate
	// before lockouts begin. Set above 1 deliberately: an operator fat-fingering
	// a password, or a bot integration restarting with a stale secret, should not
	// be locked out on the first mistake.
	adminAuthFailureThreshold = 5

	// adminAuthBaseLockout is the first lockout, doubling per subsequent failure.
	adminAuthBaseLockout = 2 * time.Second

	// adminAuthMaxLockout caps the doubling. 15 minutes reduces a brute-force
	// attempt to ~4 guesses/hour per source while still letting a locked-out
	// operator back in without a restart.
	adminAuthMaxLockout = 15 * time.Minute

	// adminAuthDecayWindow forgets a stale failure record. Without decay, two
	// typos months apart would accumulate toward a lockout forever.
	adminAuthDecayWindow = 30 * time.Minute

	// adminAuthMaxTrackedIPs bounds the state map. The map is keyed by
	// attacker-controlled source addresses, so an unbounded map would turn this
	// defence into its own memory-growth surface — the exact class of problem
	// round 17c closed on the request path. When full, the oldest record is
	// evicted; eviction only ever forgives, never locks out a new victim.
	adminAuthMaxTrackedIPs = 4096
)

// adminAuthState is one source IP's failure record.
type adminAuthState struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// adminAuthThrottle counts admin authentication failures per source IP and locks
// out sources that exceed the threshold. Safe for concurrent use.
//
// In-process only, matching rateLimiter's documented single-instance assumption:
// with N replicas an attacker gets N times the budget. Stated rather than hidden.
type adminAuthThrottle struct {
	mu     sync.Mutex
	states map[string]*adminAuthState
}

func newAdminAuthThrottle() *adminAuthThrottle {
	return &adminAuthThrottle{states: make(map[string]*adminAuthState)}
}

// adminAuthClientIP extracts the throttle key from a request.
//
// Deliberately RemoteAddr ONLY. X-Forwarded-For / X-Real-IP are attacker-supplied
// on a direct connection, so keying on them would let a single source reset its
// own counter every request by varying one header — a lockout that the attacker
// controls is not a lockout. This repo has no trusted-proxy configuration to
// validate such a header against (measured: zero occurrences of X-Forwarded-For
// handling in the tree), so there is nothing to safely trust it with.
//
// The tradeoff, stated plainly: behind a reverse proxy every request appears to
// come from the proxy, so failures aggregate across all real clients and a
// determined attacker can lock out legitimate admins from that shared address.
// That is the safer failure direction — denial of admin access is recoverable in
// adminAuthMaxLockout, whereas an unlimited guess budget against a secret that
// unlocks credential export is not. Wiring a trusted-proxy allowlist would let
// the header be honoured safely; that is a separate change.
func adminAuthClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr is not always host:port (httptest and unix sockets), so
		// fall back to the raw value rather than dropping the record entirely.
		return r.RemoteAddr
	}
	return host
}

// Allow reports whether a source may attempt admin authentication now. The
// second return is the seconds a locked-out caller must wait, for Retry-After.
func (t *adminAuthThrottle) Allow(ip string, now time.Time) (bool, int64) {
	if t == nil || ip == "" {
		return true, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.states[ip]
	if st == nil {
		return true, 0
	}
	if now.Before(st.lockedUntil) {
		remaining := int64(st.lockedUntil.Sub(now)/time.Second) + 1
		return false, remaining
	}
	// Lockout expired. Decay a stale record so an old failure streak does not
	// make the next single typo lock the operator out immediately.
	if now.Sub(st.lastFailure) > adminAuthDecayWindow {
		delete(t.states, ip)
	}
	return true, 0
}

// RecordFailure books one failed attempt and applies a lockout once the source
// crosses the threshold. The lockout doubles per failure beyond it, capped.
func (t *adminAuthThrottle) RecordFailure(ip string, now time.Time) {
	if t == nil || ip == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.states[ip]
	if st == nil {
		t.evictIfFullLocked(now)
		st = &adminAuthState{}
		t.states[ip] = st
	}
	// Decay before incrementing, so counting always reflects a recent streak
	// rather than a lifetime total.
	if !st.lastFailure.IsZero() && now.Sub(st.lastFailure) > adminAuthDecayWindow {
		st.failures = 0
	}
	st.failures++
	st.lastFailure = now

	if st.failures >= adminAuthFailureThreshold {
		over := st.failures - adminAuthFailureThreshold
		lockout := adminAuthBaseLockout
		// Shift-based doubling, bounded before the shift so a long-running
		// attack cannot overflow the duration.
		for i := 0; i < over && lockout < adminAuthMaxLockout; i++ {
			lockout *= 2
		}
		if lockout > adminAuthMaxLockout {
			lockout = adminAuthMaxLockout
		}
		st.lockedUntil = now.Add(lockout)
	}
}

// RecordSuccess clears a source's failure record. A caller that proves it holds
// the secret is not mid-brute-force, and leaving the streak in place would lock
// out a legitimate operator on their next typo.
func (t *adminAuthThrottle) RecordSuccess(ip string) {
	if t == nil || ip == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, ip)
}

// evictIfFullLocked drops the least-recently-failed record when the map is at
// capacity. Called with t.mu held.
func (t *adminAuthThrottle) evictIfFullLocked(now time.Time) {
	if len(t.states) < adminAuthMaxTrackedIPs {
		return
	}
	oldestIP := ""
	var oldest time.Time
	for ip, st := range t.states {
		// Never evict an ACTIVE lockout: that would hand an attacker a reset by
		// flooding the map with fresh source addresses.
		if now.Before(st.lockedUntil) {
			continue
		}
		if oldestIP == "" || st.lastFailure.Before(oldest) {
			oldestIP, oldest = ip, st.lastFailure
		}
	}
	if oldestIP != "" {
		delete(t.states, oldestIP)
	}
}

// rejectAdminAuthThrottled writes the 429 for a locked-out admin caller. Kept
// here so both gates emit an identical response and neither leaks whether the
// submitted password happened to be correct.
func rejectAdminAuthThrottled(w http.ResponseWriter, retryAfter int64) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if retryAfter > 0 {
		// strconv.FormatInt matches how the API-key path already emits
		// Retry-After (handler.go:721, :741) rather than inventing a second
		// integer-formatting convention for the same header.
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"Too many failed admin authentication attempts"}`))
}
