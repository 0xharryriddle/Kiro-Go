package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// A4. Both admin gates compared the shared secret in constant time but counted
// nothing, so a caller who could reach the port had an UNLIMITED guess budget
// against the password that unlocks /admin/api/config/export — the raw
// config.json, refresh tokens and ksk_ keys included.
//
// These tests are behavioural on purpose: they drive the real gates over
// httptest and assert on status codes, never on the presence of a symbol. A
// previous round (14) learned that the hard way — a test that grepped for
// "IsBedrock()" kept passing when the guard was neutralized to
// `if false && account.IsBedrock()`, because the substring still matched.

// adminThrottleHandler builds a Handler with a configured admin password and a
// live throttle. Explicit rather than via NewHandler: NewHandler builds a pool,
// trace store and background loops that this test neither needs nor should start.
func adminThrottleHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateSettingsPatch(nil, nil, "topsecret"); err != nil {
		t.Fatalf("set admin password: %v", err)
	}
	return &Handler{adminAuthThrottle: newAdminAuthThrottle()}
}

// wrongPasswordRequest is one failed guess from a fixed source address.
func wrongPasswordRequest(path, ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("X-Admin-Password", "wrong-guess")
	req.RemoteAddr = ip + ":54321"
	return req
}

// The behavioural assertion for the bot gate: a source that keeps guessing must
// eventually be refused with 429 instead of another 401. Pre-fix every attempt
// returned 401 forever.
func TestAdminBotGateLocksOutAfterRepeatedFailures(t *testing.T) {
	h := adminThrottleHandler(t)

	sawLockout := false
	for i := 0; i < adminAuthFailureThreshold+2; i++ {
		rec := httptest.NewRecorder()
		if h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", "203.0.113.7")) {
			t.Fatalf("attempt %d: a wrong password authenticated successfully", i+1)
		}
		if rec.Code == http.StatusTooManyRequests {
			sawLockout = true
			if got := rec.Header().Get("Retry-After"); got == "" {
				t.Errorf("attempt %d: 429 carried no Retry-After header", i+1)
			}
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401 or 429", i+1, rec.Code)
		}
	}

	if !sawLockout {
		t.Errorf("%d consecutive wrong passwords never produced a 429; the guess budget is unlimited",
			adminAuthFailureThreshold+2)
	}
}

// The same assertion for the /admin/api/* gate, which is the higher-value target
// (credential export lives behind it) and authenticates independently.
func TestAdminApiGateLocksOutAfterRepeatedFailures(t *testing.T) {
	h := adminThrottleHandler(t)

	sawLockout := false
	for i := 0; i < adminAuthFailureThreshold+2; i++ {
		rec := httptest.NewRecorder()
		h.handleAdminAPI(rec, wrongPasswordRequest("/admin/api/accounts", "198.51.100.9"))
		if rec.Code == http.StatusTooManyRequests {
			sawLockout = true
			break
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401 or 429", i+1, rec.Code)
		}
	}

	if !sawLockout {
		t.Errorf("%d consecutive wrong passwords never produced a 429 on /admin/api/*",
			adminAuthFailureThreshold+2)
	}
}

// The lockout is shared across both gates. Without this, an attacker simply
// alternates surfaces and gets twice the budget — which would make the whole
// defence a formality.
func TestAdminLockoutIsSharedAcrossBothGates(t *testing.T) {
	h := adminThrottleHandler(t)
	const ip = "192.0.2.44"

	// Exhaust the budget on the bot gate alone.
	for i := 0; i < adminAuthFailureThreshold+1; i++ {
		rec := httptest.NewRecorder()
		h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", ip))
	}

	// The OTHER gate must already consider this source locked out.
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, wrongPasswordRequest("/admin/api/accounts", ip))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("/admin/api/* answered %d for a source already locked out on the bot gate; want 429 (the throttle must be shared)",
			rec.Code)
	}
}

// Spoofing X-Forwarded-For must not reset the counter. If the key came from a
// caller-supplied header, the attacker controls their own lockout — i.e. there
// is no lockout. This is why adminAuthClientIP reads RemoteAddr only.
func TestAdminLockoutIgnoresForwardedForSpoofing(t *testing.T) {
	h := adminThrottleHandler(t)
	const ip = "203.0.113.99"

	for i := 0; i < adminAuthFailureThreshold+1; i++ {
		rec := httptest.NewRecorder()
		h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", ip))
	}

	// Same real source, a fresh forged forwarding header each time.
	for i, forged := range []string{"1.2.3.4", "5.6.7.8", "9.10.11.12"} {
		req := wrongPasswordRequest("/admin/new_api_key", ip)
		req.Header.Set("X-Forwarded-For", forged)
		req.Header.Set("X-Real-IP", forged)
		rec := httptest.NewRecorder()
		h.authenticateAdminKey(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("forged X-Forwarded-For %q (attempt %d) escaped the lockout: status %d, want 429",
				forged, i+1, rec.Code)
		}
	}
}

// Control: the lockout must be per source, not global. Otherwise one attacker
// locks every administrator out of the panel — a self-inflicted outage.
func TestAdminLockoutIsPerSourceAddress(t *testing.T) {
	h := adminThrottleHandler(t)

	for i := 0; i < adminAuthFailureThreshold+1; i++ {
		rec := httptest.NewRecorder()
		h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", "203.0.113.1"))
	}

	// A different source, first attempt: must get a normal 401, not the
	// neighbour's 429.
	rec := httptest.NewRecorder()
	h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", "203.0.113.2"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unrelated source got %d on its first attempt; want 401 (the lockout must not be global)", rec.Code)
	}
}

// Control: the correct password still works, and clears the streak. A safety
// feature that locks out the legitimate operator after two typos is a worse
// outage than the hole it closes.
func TestCorrectAdminPasswordStillWorksAndClearsStreak(t *testing.T) {
	h := adminThrottleHandler(t)
	const ip = "192.0.2.77"

	// A few failures, staying under the threshold.
	for i := 0; i < adminAuthFailureThreshold-1; i++ {
		rec := httptest.NewRecorder()
		h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", ip))
	}

	good := httptest.NewRequest(http.MethodPost, "/admin/new_api_key", strings.NewReader(`{}`))
	good.Header.Set("X-Admin-Password", "topsecret")
	good.RemoteAddr = ip + ":54321"
	rec := httptest.NewRecorder()
	if !h.authenticateAdminKey(rec, good) {
		t.Fatalf("the correct admin password was rejected: status %d", rec.Code)
	}

	// Streak cleared: a single later typo must be a plain 401, not a lockout.
	rec = httptest.NewRecorder()
	h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", ip))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("after a successful auth, one typo gave %d; want 401 (success must clear the streak)", rec.Code)
	}
}

// Control: a bare &Handler{} literal (169 of them exist in this suite) leaves
// adminAuthThrottle nil. Admin auth must still function — only the lockout is
// skipped — rather than panicking on a nil receiver.
func TestAdminGatesTolerateNilThrottle(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateSettingsPatch(nil, nil, "topsecret"); err != nil {
		t.Fatalf("set admin password: %v", err)
	}
	h := &Handler{} // throttle deliberately nil

	rec := httptest.NewRecorder()
	if h.authenticateAdminKey(rec, wrongPasswordRequest("/admin/new_api_key", "203.0.113.5")) {
		t.Fatal("a wrong password authenticated against a nil-throttle handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("nil throttle: status %d, want 401", rec.Code)
	}

	good := httptest.NewRequest(http.MethodPost, "/admin/new_api_key", strings.NewReader(`{}`))
	good.Header.Set("X-Admin-Password", "topsecret")
	good.RemoteAddr = "203.0.113.5:1234"
	rec = httptest.NewRecorder()
	if !h.authenticateAdminKey(rec, good) {
		t.Errorf("nil throttle rejected the correct password: status %d", rec.Code)
	}
}

// Unit: the lockout grows and is capped. Driven on the throttle directly with an
// injected clock, because asserting a 15-minute cap through HTTP would mean
// sleeping for it.
func TestAdminThrottleBackoffGrowsAndCaps(t *testing.T) {
	th := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)
	const ip = "203.0.113.30"

	// Below the threshold: no lockout.
	for i := 0; i < adminAuthFailureThreshold-1; i++ {
		th.RecordFailure(ip, now)
	}
	if allowed, _ := th.Allow(ip, now); !allowed {
		t.Fatalf("locked out after only %d failures; threshold is %d",
			adminAuthFailureThreshold-1, adminAuthFailureThreshold)
	}

	// Crossing it locks out.
	th.RecordFailure(ip, now)
	allowed, first := th.Allow(ip, now)
	if allowed {
		t.Fatalf("no lockout at the threshold (%d failures)", adminAuthFailureThreshold)
	}

	// Each further failure at least doubles, until the cap.
	th.RecordFailure(ip, now)
	_, second := th.Allow(ip, now)
	if second <= first {
		t.Errorf("backoff did not grow: %ds then %ds", first, second)
	}

	// Hammer well past the cap and confirm it is bounded.
	for i := 0; i < 40; i++ {
		th.RecordFailure(ip, now)
	}
	_, capped := th.Allow(ip, now)
	maxSeconds := int64(adminAuthMaxLockout/time.Second) + 1
	if capped > maxSeconds {
		t.Errorf("backoff %ds exceeds the %ds cap", capped, maxSeconds)
	}
}

// Unit: a stale failure record decays, so two typos months apart do not add up
// to a lockout.
func TestAdminThrottleDecaysStaleFailures(t *testing.T) {
	th := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)
	const ip = "203.0.113.31"

	for i := 0; i < adminAuthFailureThreshold; i++ {
		th.RecordFailure(ip, now)
	}
	if allowed, _ := th.Allow(ip, now); allowed {
		t.Fatal("expected a lockout at the threshold")
	}

	// Long after the decay window: the record is forgotten and the next single
	// failure must not re-lock immediately.
	later := now.Add(adminAuthDecayWindow * 3)
	if allowed, _ := th.Allow(ip, later); !allowed {
		t.Fatal("still locked out long after the lockout expired")
	}
	th.RecordFailure(ip, later)
	if allowed, _ := th.Allow(ip, later); !allowed {
		t.Error("one failure after the decay window re-locked the source; the streak did not decay")
	}
}

// Unit: the state map is bounded. It is keyed by attacker-supplied source
// addresses, so an unbounded map would make this defence its own memory-growth
// surface — the same class of problem round 17c closed on the request path.
func TestAdminThrottleBoundsTrackedSources(t *testing.T) {
	th := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < adminAuthMaxTrackedIPs+500; i++ {
		th.RecordFailure("10.0."+itoaTest(i/256)+"."+itoaTest(i%256), now)
		now = now.Add(time.Millisecond)
	}

	th.mu.Lock()
	tracked := len(th.states)
	th.mu.Unlock()
	if tracked > adminAuthMaxTrackedIPs {
		t.Errorf("throttle tracks %d sources, above the %d bound", tracked, adminAuthMaxTrackedIPs)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
