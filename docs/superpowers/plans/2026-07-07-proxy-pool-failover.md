# Proxy Pool + Proxy-Level Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a shared server proxy pool persisted in config, pick a proxy per request, and on a proxy/dial failure swap to another pool proxy while keeping the same account. Persist proxy health across restarts. Keep per-account `proxyURL` as an override. Surface the pool in the admin UI.

**Architecture:** New `PooledProxy` type + `Config.ProxyPool[]` with getters/setters. A new choke point `SelectProxyForAccount` picks: account override → pool (round-robin, skip unhealthy/disabled/cooled-down) → global proxy → require-proxy error / direct. The proxy-swap loop lives inside `CallKiroAPI`: on a transport/dial error from a pool proxy, mark it unhealthy, swap, keep the account; bounded by `maxProxySwapAttempts`, then fall through to today's account-level failover. HTTP 4xx/5xx never mark a proxy unhealthy. REST/background path gets the same via the pool-aware REST helper.

**Tech Stack:** Go stdlib `net/http` (+ `net/url`, `sync/atomic`), `github.com/google/uuid`; vanilla JS/HTML/CSS admin panel; JSON config as persistence layer.

Design: `docs/superpowers/specs/2026-07-07-proxy-pool-failover-design.md`

## Global Constraints

- Go module `kiro-go`; stdlib `net/http` only, one dep (`github.com/google/uuid`). Do not add dependencies.
- Config is the persistence layer: any new persistent setting is a `Config` field + getter/setter in `config/config.go` that calls `Save()`. Bump `Version` in `config/config.go` and update `version.json` on release.
- Never write config on the request hot path. Proxy-health writes are NOT hot-path (only on health state transition), so a direct `Save()` on transition is acceptable — but only `Save()` on a transition (healthy↔unhealthy), never on every failed attempt.
- Account selection always goes through the pool; respect cooldowns and the `excluded` set in retry loops. This plan does NOT change account selection — it changes proxy selection.
- Error classification is string-matching on upstream messages — reuse `isProxyErrorMessage` in `account_failover.go`; keep it as the single proxy-error matcher. Only transport/dial/proxy errors trigger a swap; HTTP 4xx/5xx must not mark a proxy unhealthy.
- The proxy resolution choke points are in `proxy/kiro.go`: `ResolveAccountProxyURL`, `ResolveAccountProxyURLStrict`, `restClientForAccount`. The request path moves to `SelectProxyForAccount`; `ResolveAccountProxyURLStrict` stays for non-request callers.
- Masked proxy display never shows credentials: `scheme://host:port` only (see `maskProxyForDisplay` in `web/app.js`, `maskProxyForLog` in `proxy/kiro.go`, `ParsedProxy.MaskedURL` in `proxy/proxy_import.go`).
- Comments mix English and Chinese — match the surrounding file.
- Tests live beside code (`*_test.go`) and use `auth/testhooks.go` seams to stub network calls.
- Build: `go build -o kiro-go .`  Test: `go test ./...`  Vet: `go vet ./...`

---

### Task 1: Config schema — `PooledProxy` type, `ProxyPool` field, getters/setters

**Files:**
- Modify: `config/config.go` — new type, field, getters/setters, cooldown const; bump `Version`
- Modify: `version.json` — bump + changelog
- Test: `config/config_test.go`

**Interfaces produced:**
```go
type PooledProxy struct {
    URL               string `json:"url"`
    Healthy           bool   `json:"healthy"`
    FailCount         int    `json:"failCount,omitempty"`
    LastFailAt        int64  `json:"lastFailAt,omitempty"`
    LastOKAt          int64  `json:"lastOkAt,omitempty"`
    DisabledPermanent bool   `json:"disabledPermanent,omitempty"`
}
// on Config: ProxyPool []PooledProxy `json:"proxyPool,omitempty"`

func GetProxyPool() []PooledProxy                       // returns a copy
func AddProxyToPool(url string) error                   // dedupe by URL; Healthy=true, LastOKAt=now
func RemoveProxyFromPool(url string) error
func MarkProxyUnhealthy(url string) (changed bool, err error)  // FailCount++, LastFailAt=now, Healthy=false; changed=true only on healthy→unhealthy transition
func MarkProxyHealthy(url string) (changed bool, err error)    // reset FailCount, Healthy=true, LastOKAt=now; changed=true only on unhealthy→healthy transition
func SetProxyPoolDisabled(url string, disabled bool) error
```

- [ ] **Step 1: Write failing tests** in `config/config_test.go`:
  - `TestProxyPoolAddDedupeAndRemove` — add same URL twice → one entry; remove → gone.
  - `TestMarkProxyUnhealthyHealthyTransition` — first `MarkProxyUnhealthy` returns `changed=true`; second returns `false` (already unhealthy); `MarkProxyHealthy` returns `changed=true` then `false`. Verify `FailCount`, `Healthy`, timestamps.
  - `TestProxyPoolRoundTrip` — add entries, `Save()` then reload a fresh Config from the same path, assert `ProxyPool` survives.

- [ ] **Step 2: Run tests, verify they fail** (`go test ./config/ -run TestProxyPool -v` and `-run TestMarkProxy`).

- [ ] **Step 3: Implement.** Add the type + field near the existing `ProxyURL`/`RequireProxy` block (~265). Add getters/setters guarded by the existing `sync.RWMutex`, each mutating setter calling `Save()` (except: `MarkProxyUnhealthy`/`MarkProxyHealthy` only `Save()` when the transition actually changes state — return `changed` accordingly). Add `const proxyUnhealthyCooldown = 5 * time.Minute` (exported as a package const or a getter if the pool selection needs it in another package — it lives in `proxy/`, so export `ProxyUnhealthyCooldownSeconds() int64` or a plain exported const `ProxyUnhealthyCooldown`). Follow the nil-guard pattern of `GetRequireProxy`. Bump `const Version`.

- [ ] **Step 4: Bump `version.json`** with an EN + VI changelog line describing the proxy pool + failover.

- [ ] **Step 5: Run tests, verify pass; build; vet.**

- [ ] **Step 6: Commit** — `feat(config): add shared proxy pool with persisted health`

---

### Task 2: `SelectProxyForAccount` selection choke point

**Files:**
- Modify: `proxy/kiro.go` — add `SelectProxyForAccount`, round-robin counter, eligibility helper
- Test: `proxy/kiro_test.go`

**Interfaces produced:**
```go
// SelectProxyForAccount returns the proxy URL to use and a poolKey identifying
// the chosen pool entry (empty when not from the pool), so the caller can report
// health back. Order: account override → pool (round-robin over eligible) →
// global proxy → require-proxy error / direct.
func SelectProxyForAccount(account *config.Account) (proxyURL string, poolKey string, err error)

// proxyPoolEligible reports whether a pooled proxy can be picked now: Healthy ||
// cooldown elapsed since LastFailAt; and not DisabledPermanent.
func proxyPoolEligible(p config.PooledProxy, now int64) bool
```

**Selection order (exact):**
1. `account != nil && account.ProxyURL != ""` → return `(account.ProxyURL, "", nil)`.
2. Pool: filter `config.GetProxyPool()` to eligible entries; if any, pick round-robin via a package-level `atomic.Uint64` counter modulo eligible-count → return `(entry.URL, entry.URL, nil)`.
3. `config.GetProxyURL() != ""` → return `(global, "", nil)`.
4. else if `config.GetRequireProxy()` → return `("", "", fmt.Errorf("require-proxy: no proxy configured for account"))`.
5. else → return `("", "", nil)`.

- [ ] **Step 1: Write failing tests** in `proxy/kiro_test.go`:
  - `TestSelectProxyOverrideWins` — account with `ProxyURL` set → returns it, poolKey empty, even when pool has entries.
  - `TestSelectProxyRoundRobinSkipsIneligible` — pool of 3 (one unhealthy in-cooldown, one DisabledPermanent, one healthy) → always returns the healthy one; poolKey == its URL.
  - `TestSelectProxyCooldownHalfOpen` — an unhealthy entry with `LastFailAt` older than `ProxyUnhealthyCooldown` is eligible again.
  - `TestSelectProxyGlobalFallback` — empty pool, no override, global set → returns global, poolKey empty.
  - `TestSelectProxyRequireProxyError` — empty pool, no override, no global, require-proxy on → error contains `"require-proxy"`.
  - `TestSelectProxyDirect` — same as above but require-proxy off → `("", "", nil)`.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** near `ResolveAccountProxyURLStrict`. Use `config.GetProxyPool()` (copy) and the exported cooldown from Task 1. Round-robin: `idx := proxyRRCounter.Add(1); pick eligible[(idx-1) % len(eligible)]`.

- [ ] **Step 4: Run tests, verify pass; build; vet.**

- [ ] **Step 5: Commit** — `feat(proxy): add SelectProxyForAccount pool-aware resolver`

---

### Task 3: Proxy-swap failover loop in `CallKiroAPI` + pool-aware REST helper

**Files:**
- Modify: `proxy/kiro.go` — wrap `CallKiroAPI` endpoint attempts in a proxy-swap loop; add `restClientForAccountSwapping` (or teach `restClientForAccount` to report poolKey)
- Modify: `proxy/kiro_api.go`, `proxy/kiro_overage.go` — REST calls report health on proxy error/success
- Test: `proxy/kiro_test.go`

**Constants:** `const maxProxySwapAttempts = 3` (stream), `const maxRestProxySwapAttempts = 2` (REST).

- [ ] **Step 1: Write failing test** — the swap logic is hard to drive end-to-end without a live proxy, so test the decision helper, not the network:
  - Extract the swap decision into a pure helper `shouldSwapProxy(transportErr error, poolKey string, attempts int) bool` = `isProxyErrorMessage(err.Error()) && poolKey != "" && attempts < maxProxySwapAttempts`. Test: proxy error + poolKey + under cap → true; non-proxy error → false; empty poolKey → false; at cap → false.
  - `TestMarkProxyHealthyNotCalledOnHTTPError` is covered by Task 2/1 units; here assert `shouldSwapProxy` returns false for an HTTP-status error string (e.g. `"HTTP 401 ..."`).

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement.**
  - In `CallKiroAPI`, replace the single `SelectProxyForAccount`/`GetClientForProxy` resolution with a loop: resolve, run the existing endpoint attempts; capture the transport error (the `resp, err := proxyClient.Do(req)` error, NOT an HTTP status). After the endpoint loop, if `shouldSwapProxy(lastTransportErr, poolKey, proxyAttempts)`: `config.MarkProxyUnhealthy(poolKey)`, `proxyAttempts++`, `continue`. On a fully successful request through a pool proxy (`poolKey != ""`), call `config.MarkProxyHealthy(poolKey)` once. If swaps exhaust, return the proxy error so the existing account-level failover (Task 5 of prior plan) takes over.
  - Keep the `[Route]` log line; log the chosen proxy each swap iteration.
  - Preserve `payload.ProfileArn` defer/restore and the strict gate ordering (proxy resolution before `ResolveProfileArn` network call — the require-proxy error path must still short-circuit before any egress).
  - REST helper: add a variant that returns `(client, poolKey, err)` and have `kiro_api.go`/`kiro_overage.go` mark health on proxy error/success within a bounded `maxRestProxySwapAttempts` loop. Keep the change minimal — a small local loop per call site or one shared helper `doRESTWithProxySwap(account, buildReq func() (*http.Request,error)) (*http.Response, error)`. Prefer the shared helper to avoid duplicating the swap logic across 6 sites.

- [ ] **Step 4: Run tests, verify pass; build; vet.** Run the existing proxy suite; confirm only the two known pre-existing `translator_test.go` failures remain.

- [ ] **Step 5: Commit** — `feat(proxy): swap to another pool proxy on transport failure, keep account`

---

### Task 4: Pool admin endpoints

**Files:**
- Modify: `proxy/handler.go` — route + handlers under `/admin/api/proxy/pool`
- Test: manual (admin endpoints have no unit seam) — build-verified; exercised in Task 6 manual step

**Endpoints:**
- `GET /admin/api/proxy/pool` → `{pool: [{url(masked), healthy, failCount, lastFailAt, lastOkAt, disabledPermanent}]}` — mask credentials in the URL before returning.
- `POST /admin/api/proxy/pool` → body `{url}` or `{urls:[...]}` → `AddProxyToPool` each (validate scheme prefix like `apiUpdateProxy` does); return counts.
- `DELETE /admin/api/proxy/pool` → body `{url}` → `RemoveProxyFromPool`.
- `POST /admin/api/proxy/pool/toggle` → body `{url, disabled}` → `SetProxyPoolDisabled`.

- [ ] **Step 1: Find the admin route dispatch** (`handleAdminAPI`, `strings.TrimPrefix(r.URL.Path, "/admin/api")` ~2182) and the `apiGetProxy`/`apiUpdateProxy` handlers (~4137) for the pattern.

- [ ] **Step 2: Add the route cases** matching existing style (method check, password already gated upstream). Reuse the URL scheme validation from `apiUpdateProxy`. Return masked URLs on GET (never credentials) — reuse `maskProxyForLog` or a small mask that yields `scheme://host:port`.

- [ ] **Step 3: Build; vet.**

- [ ] **Step 4: Commit** — `feat(admin): proxy pool CRUD endpoints`

---

### Task 5: Import "add to pool" mode

**Files:**
- Modify: `proxy/proxy_import.go` — add a mode that parses+probes lines and adds to the pool instead of assigning to accounts
- Modify: `proxy/handler.go` — the import endpoint accepts a `target: "accounts" | "pool"` flag
- Test: `proxy/proxy_import_test.go` if present, else build-verified + manual

- [ ] **Step 1: Locate** `ImportAndAssignProxies` (`proxy/proxy_import.go:242`) and its handler.

- [ ] **Step 2: Implement** a sibling path: when target=="pool", for each parsed+probed line call `config.AddProxyToPool(p.fullURL(scheme))` and report reachable/added per line (reuse `ImportProxiesResult`, set `Assigned=false`, add an `AddedToPool bool` field). Do not assign to accounts in this mode. Keep the existing accounts mode unchanged (default).

- [ ] **Step 3: Handler** — read the `target` field (default `"accounts"`), dispatch accordingly.

- [ ] **Step 4: Build; vet; run any import test.**

- [ ] **Step 5: Commit** — `feat(proxy): import proxies into the shared pool`

---

### Task 6: UI — pool list, import mode toggle, i18n, CSS

**Files:**
- Modify: `web/index.html` — pool list section + import mode toggle in the merged Proxy card
- Modify: `web/app.js` — load/render pool, add/remove/toggle handlers, import target toggle
- Modify: `web/styles.css` — health dot + pool row styles
- Modify: `web/locales/en.json`, `zh.json`, `vi.json` — new keys

**Element IDs (new):** `proxyPoolList`, `proxyPoolAddInput`, `proxyPoolAddBtn`, `proxyImportTarget` (select: accounts/pool). Keep all existing IDs unchanged.

**i18n keys (new):** `settings.proxyPool`, `settings.proxyPoolAdd`, `settings.proxyPoolEmpty`, `settings.proxyPoolHealthy`, `settings.proxyPoolUnhealthy`, `settings.proxyPoolDisabled`, `settings.proxyPoolRemove`, `settings.proxyPoolFailCount`, `proxyImport.targetAccounts`, `proxyImport.targetPool`.

- [ ] **Step 1: Markup** — add a "Proxy Pool" sub-section in the merged card (below require-proxy, above or beside Import): an add-input + button and a `#proxyPoolList` container. Add a `#proxyImportTarget` select to the Import sub-section.

- [ ] **Step 2: JS** — `loadProxyPool()` (GET), `renderProxyPool()` (masked host:port + colored health dot + fail count + remove + enable/disable), `addProxyToPool()`, `removeProxyFromPool()`, `toggleProxyPool()`. Wire `importProxies` to send `target: $('proxyImportTarget').value`. Refresh the pool list after import when target==pool. Health dot: green healthy, red unhealthy, grey disabled. Reuse `maskProxyForDisplay`.

- [ ] **Step 3: CSS** — `.proxy-pool-row`, `.proxy-health-dot` (+ `.healthy/.unhealthy/.disabled` modifiers). Match existing token style.

- [ ] **Step 4: i18n** — add the keys to all three locales (EN, ZH, VI), positional `{0}` for fail count.

- [ ] **Step 5: Verify** — `go build`, JSON-validate all three locales, JS syntax check, no null bytes; run the app and confirm: pool add/remove/toggle work, health dots render, import-to-pool adds entries, existing account-assign import still works, merged card unchanged otherwise.

- [ ] **Step 6: Commit** — `feat(web): proxy pool management UI + import target toggle`

---

## Final review

After Task 6: dispatch the whole-branch code reviewer (most capable model) with the design doc + full feature diff. Pay special attention to: (a) proxy health writes never on the hot path except on transition; (b) HTTP status errors never mark a proxy unhealthy; (c) credentials never surface in any pool GET response or UI; (d) require-proxy short-circuit still precedes all egress; (e) round-robin counter has no data race.
