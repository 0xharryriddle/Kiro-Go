package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// ============================================================================
// Route-equivalence harness (F1 second half, step 1)
//
// WHY THIS EXISTS
//
// ServeHTTP dispatches with a 91-arm `switch { case ... }` (handler.go:682).
// That construct is FIRST-MATCH and therefore ORDER-SENSITIVE, and several arms
// are only correct because of where they sit:
//
//   - `/admin/new_api_key` (and the other eight admin-key POST routes) MUST be
//     tested before `strings.HasPrefix(path, "/admin/")`, or they get served as
//     static files instead of hitting the admin-key gate. The switch even carries
//     a comment saying so.
//   - `/admin/api/login` and `/admin/api/logout` MUST be tested before
//     `strings.HasPrefix(path, "/admin/api/")`, because handleAdminAPI sits behind
//     the password gate and login is how you GET a session in the first place.
//
// Converting that switch to a route table reorders those comparisons by
// construction. A table keyed by exact path plus a separate prefix list does not
// preserve "first arm wins" unless the conversion is done deliberately, and the
// failure mode is silent: the server still starts, still serves, and the only
// symptom is that a route quietly resolves to the wrong handler. A static-file
// 404 where a 401 belongs is an availability bug; a 200 where a gate belongs is
// a security bug.
//
// So this harness pins observable dispatch BEFORE any routing change, and is
// committed on its own. It is deliberately a characterization test: it asserts
// what the code does today, not what it ought to do. Two of the golden values
// below are arguably wrong on their merits (see the notes in the table) and are
// pinned anyway — the point is to make a refactor prove it changed nothing, so
// behaviour questions stay separate from mechanical questions.
//
// WHAT IT DOES NOT COVER
//
// Fingerprints are observable HTTP results, not handler identities. Two arms
// that answer identically (e.g. several unauthenticated routes that all answer
// 401 with the same body) are indistinguishable here, so this cannot catch a
// swap BETWEEN two such arms. It catches order/precedence regressions and any
// change that alters status, content-type, or body shape. Instrumenting the
// handler set to record which function ran would close that gap and is the
// natural next step if the table conversion turns out to need it.
//
// NETWORK AND ACCOUNT SAFETY
//
// Auth is enabled with zero keys configured, which makes authenticate() fail
// closed (proxy/auth.go: "Auth required but nothing configured → fail closed"),
// so every guarded route answers 401 without selecting an account or calling
// upstream. refreshModelsHook is stubbed because /v1/models is unauthenticated
// and would otherwise sweep every enabled account via ListAvailableModels.
// ============================================================================

// routeProbe is one (method, path) pair driven through the real ServeHTTP.
type routeProbe struct {
	method string
	path   string
	// why documents what this probe is defending, for probes whose value is not
	// self-evident. Empty for plain coverage probes.
	why string
}

// routeDigits normalizes digit runs so timestamps/uptimes (/health, /healthz)
// do not make the fingerprint time-dependent.
var routeDigits = regexp.MustCompile(`[0-9]+`)

// routeFingerprint captures the observable result of dispatching one probe:
// status, content-type, and a normalized body prefix. Body shape is what
// separates handlers that happen to share a status code — e.g. the admin-key
// gate's JSON 401 vs handleAdminAPI's 401 vs a static-file 404.
func routeFingerprint(t *testing.T, h *Handler, p routeProbe) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(p.method, p.path, nil)
	// Fixed RemoteAddr: resolveClientIP feeds the admin brute-force guard, and a
	// varying IP would make lockout state depend on probe order.
	req.RemoteAddr = "203.0.113.7:54321"

	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if len(body) > 72 {
		body = body[:72]
	}
	body = strings.ReplaceAll(body, "\r", "")
	body = strings.ReplaceAll(body, "\n", `\n`)
	body = routeDigits.ReplaceAllString(body, "N")

	return fmt.Sprintf("%d | %s | %s", rec.Code, rec.Header().Get("Content-Type"), body)
}

// sentinelWebRoot chdirs into a throwaway directory holding web/ assets whose
// contents are unique per file.
//
// WHY THIS IS NECESSARY, not incidental setup: the file-serving arms resolve
// relative paths ("web/index.html", "web/portal.html", "web/usage.html",
// "web/"+path) against the process cwd, which during `go test` is the package
// directory — where no web/ exists. Every one of those arms therefore answered an
// identical `404 page not found`, so seven distinct routes shared one
// fingerprint and the harness could not tell them apart. A table conversion that
// pointed /check at serveAdminPage would have passed.
//
// Serving the REAL repo assets (chdir to ..) would fix the ambiguity but couple
// routing goldens to frontend HTML, so an unrelated edit to index.html would
// fail this test. Sentinels keep the coupling where it belongs: on the routing.
//
// os.Chdir rather than t.Chdir: t.Chdir landed in Go 1.24 and go.mod declares
// go 1.21, so vet's stdversion analyzer rejects it. No test in this package calls
// t.Parallel(), so mutating process cwd and restoring it is safe here.
func sentinelWebRoot(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatalf("mkdir web: %v", err)
	}
	// Distinct first bytes per file: the fingerprint's body prefix is what
	// separates these arms, so identical placeholder text would defeat the point.
	for name, body := range map[string]string{
		"index.html":  "<!doctype html><title>SENTINEL-ADMIN-INDEX</title>",
		"portal.html": "<!doctype html><title>SENTINEL-CHECK-PORTAL</title>",
		"usage.html":  "<!doctype html><title>SENTINEL-USAGE-PAGE</title>",
		"app.js":      "/* SENTINEL-ADMIN-STATIC-APPJS */",
	} {
		if err := os.WriteFile(filepath.Join(root, "web", name), []byte(body), 0o644); err != nil {
			t.Fatalf("write web/%s: %v", name, err)
		}
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir to sentinel root: %v", err)
	}
	// Restore unconditionally: leaking cwd would corrupt every later test in the
	// package, and the failure would surface far from its cause.
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
}

// newRouteHarness builds a Handler wired just enough for every arm of the switch
// to be reachable without network access or a real account.
func newRouteHarness(t *testing.T) *Handler {
	t.Helper()

	// Must precede config.Init: readyz probes writability of a relative "data"
	// dir, and this keeps that write inside the throwaway root instead of
	// creating proxy/data/ in the repo.
	sentinelWebRoot(t)

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Auth ON, zero keys => fail closed. Admin password non-empty so the admin
	// gate is a real comparison rather than the ""=="" fail-open that
	// admin_auth_failclosed_test.go covers.
	if err := config.UpdateSettings("", true, "route-harness-password"); err != nil {
		t.Fatalf("config.UpdateSettings: %v", err)
	}

	p := accountpool.GetPool()
	// Reload against the fresh empty config so no account leaks in from an
	// earlier test in this package.
	p.Reload()

	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		rpmThrottle: newRPMThrottle(),
		// maxFails=0 disables lockout, so repeated admin probes cannot start
		// answering 429 partway through the table.
		adminGuard:    newAdminAuthGuard(0, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Minute),
		// Keep /v1/models off the network and off the account fleet.
		refreshModelsHook: func() {},
	}
}

// routeProbes enumerates every arm of the ServeHTTP switch plus the precedence
// hazards that a table conversion is most likely to break.
func routeProbes() []routeProbe {
	return []routeProbe{
		// ---- operational endpoints -------------------------------------------
		{method: http.MethodGet, path: "/healthz"},
		{method: http.MethodGet, path: "/readyz"},
		{method: http.MethodGet, path: "/metrics", why: "gated by config.MetricsEnabled; 404 when off so a probe cannot fingerprint it"},

		// ---- Claude surface (all aliases) ------------------------------------
		{method: http.MethodPost, path: "/v1/messages"},
		{method: http.MethodPost, path: "/messages"},
		{method: http.MethodPost, path: "/anthropic/v1/messages"},
		{method: http.MethodPost, path: "/v1/messages/count_tokens"},
		{method: http.MethodPost, path: "/messages/count_tokens"},

		// ---- OpenAI surface (all aliases) ------------------------------------
		{method: http.MethodPost, path: "/v1/chat/completions"},
		{method: http.MethodPost, path: "/chat/completions"},
		{method: http.MethodPost, path: "/v1/responses"},
		{method: http.MethodPost, path: "/responses"},

		// ---- models ----------------------------------------------------------
		{method: http.MethodGet, path: "/v1/models", why: "unauthenticated by design"},
		{method: http.MethodGet, path: "/models"},

		// ---- customer self-service ------------------------------------------
		{method: http.MethodGet, path: "/v1/key/info"},
		{method: http.MethodGet, path: "/key/info"},
		{method: http.MethodGet, path: "/v1/key/logs"},
		{method: http.MethodGet, path: "/key/logs"},
		{method: http.MethodPost, path: "/api/event_logging/batch", why: "Claude Code telemetry sink: must stay an unconditional 200"},
		{method: http.MethodGet, path: "/api/stats"},
		{method: http.MethodPost, path: "/api/stats", why: "same route accepts GET and POST"},
		{method: http.MethodGet, path: "/api/me"},
		{method: http.MethodPost, path: "/api/me"},
		{method: http.MethodGet, path: "/api/logs"},
		{method: http.MethodPost, path: "/api/logs"},

		// ---- method guards: wrong verb must fall through to 404 -------------
		{method: http.MethodDelete, path: "/api/stats", why: "arm is method-qualified; DELETE must reach default, not the handler"},
		{method: http.MethodDelete, path: "/api/me", why: "method-qualified arm"},
		{method: http.MethodDelete, path: "/api/logs", why: "method-qualified arm"},

		// ---- admin-key POST routes: PRECEDENCE HAZARD -----------------------
		// Each of these must beat HasPrefix("/admin/"). If a conversion lets the
		// prefix win, these become static-file lookups: 404 instead of the
		// admin-key gate's 401, and the bot integration silently dies.
		{method: http.MethodPost, path: "/admin/new_api_key", why: "PRECEDENCE: must beat HasPrefix(/admin/) -> admin-key gate, not static file"},
		{method: http.MethodPost, path: "/admin/delete_api_key", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodPost, path: "/admin/recharge_api_key", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodPost, path: "/admin/stats", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodGet, path: "/admin/pool", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodPost, path: "/admin/add_kiro_api_key", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodPost, path: "/admin/add_kiro_account", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodPost, path: "/admin/add_custom_api_account", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},
		{method: http.MethodPost, path: "/admin/add_bedrock_account", why: "PRECEDENCE: must beat HasPrefix(/admin/)"},

		// Wrong verb on a method-qualified admin route falls to the static-file
		// prefix arm, NOT to the admin gate. Pinning this because it is the
		// exact behaviour a table conversion tends to "tidy up" by accident.
		{method: http.MethodGet, path: "/admin/new_api_key", why: "method-qualified: GET falls through to the static-file prefix arm"},

		// ---- admin session routes: PRECEDENCE HAZARD ------------------------
		// GET is the discriminating probe: handleAdminLogin answers 405, while
		// handleAdminAPI (the prefix arm) answers 401. If login/logout lose their
		// position ahead of HasPrefix("/admin/api/"), nobody can authenticate.
		{method: http.MethodGet, path: "/admin/api/login", why: "PRECEDENCE: 405 proves handleAdminLogin ran; 401 would mean the prefix arm swallowed it"},
		{method: http.MethodPost, path: "/admin/api/login", why: "wrong password path"},
		{method: http.MethodGet, path: "/admin/api/logout", why: "PRECEDENCE: must beat HasPrefix(/admin/api/)"},
		{method: http.MethodPost, path: "/admin/api/logout"},

		// ---- admin API + static prefixes ------------------------------------
		{method: http.MethodGet, path: "/admin/api/accounts", why: "prefix arm: password gate"},
		{method: http.MethodGet, path: "/admin/api/nonexistent-probe", why: "prefix arm: gate runs before route lookup"},
		{method: http.MethodGet, path: "/admin", why: "admin page"},
		{method: http.MethodGet, path: "/admin/", why: "admin page (trailing slash)"},
		{method: http.MethodGet, path: "/admin/app.js", why: "static-file prefix arm"},

		// ---- portal / usage --------------------------------------------------
		{method: http.MethodGet, path: "/check"},
		{method: http.MethodGet, path: "/check/"},
		{method: http.MethodGet, path: "/usage", why: "GET serves the page"},
		{method: http.MethodGet, path: "/usage/"},
		{method: http.MethodPost, path: "/usage", why: "POST is the self-service API on the same path"},
		{method: http.MethodPost, path: "/usage/"},
		{method: http.MethodPost, path: "/v1/usage", why: "POST-only alias: has no GET arm"},
		{method: http.MethodGet, path: "/v1/usage", why: "GET on a POST-only alias must reach default 404"},
		{method: http.MethodDelete, path: "/usage", why: "neither GET nor POST arm matches"},

		// ---- health / root ---------------------------------------------------
		{method: http.MethodGet, path: "/health"},
		{method: http.MethodGet, path: "/", why: "root is aliased to /health"},

		// ---- stats -----------------------------------------------------------
		{method: http.MethodGet, path: "/v1/stats", why: "API-key gated"},

		// ---- CORS / preflight ------------------------------------------------
		// OPTIONS short-circuits at 204 before routing, and the CORS headers are
		// withheld from /admin on purpose (a wildcard ACAO there would let any
		// site script a logged-in operator's admin API).
		{method: http.MethodOptions, path: "/v1/messages", why: "preflight short-circuit, public surface"},
		{method: http.MethodOptions, path: "/admin/api/accounts", why: "preflight on admin: must NOT carry wildcard CORS"},

		// ---- default ---------------------------------------------------------
		{method: http.MethodGet, path: "/definitely-not-a-route"},
		{method: http.MethodGet, path: "/v1/unknown"},
		{method: http.MethodGet, path: "/adminx", why: "near-miss on the /admin prefix"},
	}
}

// TestRouteDispatchMatchesGolden pins observable dispatch for every route.
//
// Run with KIROGO_ROUTE_CAPTURE=1 to print the current table; that output is
// what gets pasted into routeGolden below. Capture mode exists so the golden
// values are recorded FROM the live switch instead of being guessed, and it
// fails the test on purpose so a capture run can never be mistaken for a pass.
func TestRouteDispatchMatchesGolden(t *testing.T) {
	h := newRouteHarness(t)
	probes := routeProbes()

	if os.Getenv("KIROGO_ROUTE_CAPTURE") == "1" {
		var b strings.Builder
		b.WriteString("\n==== ROUTE FINGERPRINT CAPTURE ====\n")
		for _, p := range probes {
			b.WriteString(fmt.Sprintf("\t%q: %q,\n", p.method+" "+p.path, routeFingerprint(t, h, p)))
		}
		b.WriteString("==== END CAPTURE ====\n")
		t.Fatalf("%s", b.String())
	}

	if len(routeGolden) == 0 {
		t.Fatal("routeGolden is empty: run with KIROGO_ROUTE_CAPTURE=1 and paste the captured table")
	}

	for _, p := range probes {
		key := p.method + " " + p.path
		want, ok := routeGolden[key]
		if !ok {
			t.Errorf("no golden fingerprint for %q (probe added without recapturing?)", key)
			continue
		}
		got := routeFingerprint(t, h, p)
		if got != want {
			msg := fmt.Sprintf("ROUTE DISPATCH CHANGED for %q\n  want: %s\n  got:  %s", key, want, got)
			if p.why != "" {
				msg += fmt.Sprintf("\n  this probe defends: %s", p.why)
			}
			t.Error(msg)
		}
	}

	// Guard against silent shrinkage: a probe deleted from routeProbes() would
	// otherwise reduce coverage without any test failing.
	if len(routeGolden) != len(probes) {
		t.Errorf("probe/golden count drift: %d probes vs %d golden entries", len(probes), len(routeGolden))
	}
}

// statusOf drives one probe and returns just the status code, for assertions that
// care about which handler ran rather than exactly what it printed.
func statusOf(t *testing.T, h *Handler, method, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "203.0.113.7:54321"
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestRoutePrecedenceHazardsStayDistinct asserts the order-sensitive properties
// RELATIONALLY, so they survive a careless regeneration of routeGolden.
//
// The golden table is the broad net; this is the part that must never break. Each
// assertion below names the wrong-handler status explicitly, because "not equal to
// the right value" is a weak claim while "must not be the static-file 404" pins
// the actual failure mode a table conversion produces.
func TestRoutePrecedenceHazardsStayDistinct(t *testing.T) {
	h := newRouteHarness(t)

	// --- hazard 1: admin-key POST routes vs HasPrefix("/admin/") --------------
	// With correct ordering these reach authenticateAdminKey and answer 401.
	// If the static-file prefix arm wins instead, they answer 404 and every bot
	// integration (Telegram key provisioning, recharge, stats) silently breaks.
	adminKeyRoutes := []string{
		"/admin/new_api_key",
		"/admin/delete_api_key",
		"/admin/recharge_api_key",
		"/admin/stats",
		"/admin/add_kiro_api_key",
		"/admin/add_kiro_account",
		"/admin/add_custom_api_account",
		"/admin/add_bedrock_account",
	}
	for _, path := range adminKeyRoutes {
		got := statusOf(t, h, http.MethodPost, path)
		if got == http.StatusNotFound {
			t.Errorf("PRECEDENCE BROKEN: POST %s answered 404 — the HasPrefix(\"/admin/\") static-file arm "+
				"is now matching before the admin-key route, so this endpoint is served as a missing file "+
				"instead of hitting the admin gate", path)
			continue
		}
		if got != http.StatusUnauthorized {
			t.Errorf("POST %s: want 401 from the admin-key gate, got %d", path, got)
		}
	}
	// GET /admin/pool is the one admin-key route that is not a POST.
	if got := statusOf(t, h, http.MethodGet, "/admin/pool"); got != http.StatusUnauthorized {
		t.Errorf("GET /admin/pool: want 401 from the admin-key gate, got %d", got)
	}

	// --- hazard 2: login/logout vs HasPrefix("/admin/api/") ------------------
	// GET is the discriminating verb. handleAdminLogin rejects non-POST with 405;
	// handleAdminAPI (the prefix arm) would answer 401 because no password was
	// supplied. So 401 here means login got swallowed by the prefix arm and
	// NOBODY CAN LOG IN — the panel is bricked while every test that only checks
	// "did it 4xx" still passes.
	if got := statusOf(t, h, http.MethodGet, "/admin/api/login"); got != http.StatusMethodNotAllowed {
		t.Errorf("PRECEDENCE BROKEN: GET /admin/api/login answered %d, want 405. A 401 means the "+
			"HasPrefix(\"/admin/api/\") password gate matched before the login route, which makes "+
			"authentication impossible: you need the session the login route mints in order to pass "+
			"the gate that is now shadowing it", got)
	}
	// logout must also stay reachable without a session, else a stale-cookie
	// client can never clear its own state.
	if got := statusOf(t, h, http.MethodGet, "/admin/api/logout"); got != http.StatusOK {
		t.Errorf("PRECEDENCE BROKEN: GET /admin/api/logout answered %d, want 200 — logout must be "+
			"reachable without already holding a valid session", got)
	}

	// --- the prefix arms themselves must still gate ---------------------------
	// Counterpart to the two hazards above: proving login/logout escape the gate
	// is only meaningful if the gate still catches everything else. A 404 here
	// would mean the prefix arm stopped matching; a 200 would mean it stopped
	// gating (full credential export is behind this).
	for _, path := range []string{"/admin/api/accounts", "/admin/api/nonexistent-probe"} {
		if got := statusOf(t, h, http.MethodGet, path); got != http.StatusUnauthorized {
			t.Errorf("GET %s: want 401 from the admin password gate, got %d — the /admin/api/ prefix "+
				"arm is no longer gating this path", path, got)
		}
	}

	// --- hazard 3: method-qualified arms must not swallow other verbs ---------
	// These arms are `path == X && method == Y`. Dropping the method predicate in
	// a table conversion would route DELETE into a GET/POST handler.
	for _, path := range []string{"/api/stats", "/api/me", "/api/logs"} {
		if got := statusOf(t, h, http.MethodDelete, path); got != http.StatusNotFound {
			t.Errorf("DELETE %s answered %d, want 404: the arm is method-qualified (GET/POST only), "+
				"so an unlisted verb must fall through to the default", path, got)
		}
	}
	// /v1/usage exists only as a POST alias; a GET must not find it.
	if got := statusOf(t, h, http.MethodGet, "/v1/usage"); got != http.StatusNotFound {
		t.Errorf("GET /v1/usage answered %d, want 404: /v1/usage is POST-only and has no GET arm", got)
	}
	// ...while POST /v1/usage must still reach the self-service handler.
	if got := statusOf(t, h, http.MethodPost, "/v1/usage"); got != http.StatusUnauthorized {
		t.Errorf("POST /v1/usage answered %d, want 401 from the self-service key check", got)
	}
}

// TestRouteAliasesStayUnified asserts that paths documented as aliases keep
// dispatching identically.
//
// This is the complement to the precedence test: that one checks routes stay
// SEPARATE, this one checks aliases stay JOINED. A table conversion that maps
// "/messages" to a different handler than "/v1/messages" would leave both
// answering 4xx during an unauthenticated test run and slip past a status-only
// check, so these compare full fingerprints.
func TestRouteAliasesStayUnified(t *testing.T) {
	h := newRouteHarness(t)

	groups := []struct {
		why    string
		method string
		paths  []string
	}{
		{"Claude messages aliases", http.MethodPost,
			[]string{"/v1/messages", "/messages", "/anthropic/v1/messages"}},
		{"count_tokens aliases", http.MethodPost,
			[]string{"/v1/messages/count_tokens", "/messages/count_tokens"}},
		{"OpenAI chat aliases", http.MethodPost,
			[]string{"/v1/chat/completions", "/chat/completions"}},
		{"OpenAI responses aliases", http.MethodPost,
			[]string{"/v1/responses", "/responses"}},
		{"models aliases", http.MethodGet,
			[]string{"/v1/models", "/models"}},
		{"key info aliases", http.MethodGet,
			[]string{"/v1/key/info", "/key/info"}},
		{"key logs aliases", http.MethodGet,
			[]string{"/v1/key/logs", "/key/logs"}},
		{"root is aliased to /health", http.MethodGet,
			[]string{"/health", "/"}},
		{"admin page tolerates a trailing slash", http.MethodGet,
			[]string{"/admin", "/admin/"}},
		{"portal tolerates a trailing slash", http.MethodGet,
			[]string{"/check", "/check/"}},
		{"usage page tolerates a trailing slash", http.MethodGet,
			[]string{"/usage", "/usage/"}},
		{"usage self-service POST aliases", http.MethodPost,
			[]string{"/usage", "/usage/", "/v1/usage"}},
	}

	for _, g := range groups {
		first := routeFingerprint(t, h, routeProbe{method: g.method, path: g.paths[0]})
		for _, p := range g.paths[1:] {
			got := routeFingerprint(t, h, routeProbe{method: g.method, path: p})
			if got != first {
				t.Errorf("ALIAS SPLIT (%s): %s %s and %s %s no longer dispatch identically\n  %s: %s\n  %s: %s",
					g.why, g.method, g.paths[0], g.method, p, g.paths[0], first, p, got)
			}
		}
	}
}

// TestAdminPreflightWithholdsWildcardCORS pins the one dispatch property that is
// a header rather than a body: the public API surface is cross-origin callable,
// the admin surface deliberately is not.
func TestAdminPreflightWithholdsWildcardCORS(t *testing.T) {
	h := newRouteHarness(t)

	pub := httptest.NewRecorder()
	h.ServeHTTP(pub, httptest.NewRequest(http.MethodOptions, "/v1/messages", nil))
	if got := pub.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("public surface lost its wildcard CORS header: got %q, want %q", got, "*")
	}

	adm := httptest.NewRecorder()
	h.ServeHTTP(adm, httptest.NewRequest(http.MethodOptions, "/admin/api/accounts", nil))
	if got := adm.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("admin surface gained a cross-origin CORS header (%q) — any website could then script "+
			"the admin API against a logged-in operator's browser", got)
	}
}
