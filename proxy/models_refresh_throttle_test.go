package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// GET /v1/models is UNAUTHENTICATED (see the route table in handleHTTP: it has
// no authenticate* call, unlike /v1/messages and /v1/chat/completions). That is
// deliberate — clients discover the model list before presenting a key — but it
// means anyone who can reach the port controls how often handleModels runs.
//
// handleModels refreshes whenever the cache is EMPTY, and refreshModelsCache
// "always replaces the aggregate, including with an empty result"
// (handler.go:1138-1144). So when every account is failing, the cache stays
// empty and every single request triggers another full refresh — and each
// refresh calls handleAccountFailure per account (handler.go, the two
// ensureValidToken / ListAvailableModels error branches).
//
// Chained together that is an unauthenticated amplifier: N requests produce
// N × accounts upstream probes and N × accounts error-counter increments, which
// drives cooldowns and — for anything classified as an auth failure — bans. The
// fleet is the scarce resource this proxy exists to protect, so an anonymous
// caller must not be able to burn it.
//
// modelsCacheTime is written in three places and NEVER compared in production
// code, so it provided no protection at all.
//
// The fix is a minimum interval between refresh attempts. It must not weaken the
// "don't serve stale models" property: a SUCCESSFUL refresh still replaces the
// aggregate immediately, and invalidateAggregatedModelsCache still forces the
// next request to refresh. Only repeated FAILING refreshes are throttled.

// TestModelsRefreshIsThrottledWhenCacheStaysEmpty pins the amplification bound.
func TestModelsRefreshIsThrottledWhenCacheStaysEmpty(t *testing.T) {
	mustInitConfig(t)

	var refreshes int64
	h := &Handler{}
	h.refreshModelsHook = func() { atomic.AddInt64(&refreshes, 1) }

	// Ten unauthenticated requests in immediate succession, cache never fills
	// (no accounts configured, so the aggregate stays empty — exactly the
	// all-accounts-failing shape).
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (model discovery must keep working)", i, rec.Code)
		}
	}

	got := atomic.LoadInt64(&refreshes)
	if got > 1 {
		t.Fatalf("10 unauthenticated /v1/models requests triggered %d upstream refreshes; "+
			"each one probes and penalises EVERY account, so an anonymous caller can drive "+
			"cooldowns and bans at will", got)
	}
}

// The throttle must not break the feature: once the interval elapses, a refresh
// is allowed again, or a genuinely empty cache would never recover.
func TestModelsRefreshResumesAfterThrottleInterval(t *testing.T) {
	mustInitConfig(t)

	var refreshes int64
	h := &Handler{}
	h.refreshModelsHook = func() { atomic.AddInt64(&refreshes, 1) }

	rec := httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if atomic.LoadInt64(&refreshes) != 1 {
		t.Fatalf("first request must attempt a refresh, got %d", atomic.LoadInt64(&refreshes))
	}

	// Rewind the last-attempt clock past the interval.
	//
	// Note the units: modelsRefreshAttemptedAt is UNIX SECONDS and
	// modelsRefreshMinInterval is a plain second count, so the arithmetic has to
	// happen in seconds. Writing time.Now().Add(-2*modelsRefreshMinInterval)
	// would subtract 120 NANOSECONDS (Add takes a Duration), leaving the stamp
	// effectively unchanged and making this test fail for a reason that has
	// nothing to do with the throttle.
	h.modelsCacheMu.Lock()
	h.modelsRefreshAttemptedAt = time.Now().Unix() - 2*modelsRefreshMinInterval
	h.modelsCacheMu.Unlock()

	rec = httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := atomic.LoadInt64(&refreshes); got != 2 {
		t.Fatalf("refresh count = %d, want 2: an empty cache must still recover once the interval elapses", got)
	}
}

// Explicit invalidation (profile switch, region change) must bypass the throttle
// entirely — otherwise switching a profile could serve models from the OLD
// profile for up to the interval, which is the stale-model bug the
// always-replace comment exists to prevent.
func TestInvalidateBypassesModelsRefreshThrottle(t *testing.T) {
	mustInitConfig(t)

	var refreshes int64
	h := &Handler{}
	h.refreshModelsHook = func() { atomic.AddInt64(&refreshes, 1) }

	rec := httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	first := atomic.LoadInt64(&refreshes)

	// A second immediate request is throttled...
	rec = httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if atomic.LoadInt64(&refreshes) != first {
		t.Fatalf("second immediate request should have been throttled")
	}

	// ...but an explicit invalidation must clear the throttle.
	h.invalidateAggregatedModelsCache()
	rec = httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := atomic.LoadInt64(&refreshes); got != first+1 {
		t.Fatalf("refresh count = %d, want %d: explicit invalidation must bypass the throttle "+
			"so a profile switch cannot serve models from the previous profile", got, first+1)
	}
}
