# Proxy pool + proxy-level failover — design

Date: 2026-07-07

## Problem

Today each account carries one `proxyURL`. On a proxy/dial failure the retry
loop cools down the **account** and rotates to a different account (Task 5).
That wastes a healthy account when only its proxy is dead, and there is no
shared inventory of proxies to draw from.

User wants:
1. A **shared server proxy pool** persisted in config.
2. **Pick-per-request** selection from the pool.
3. On proxy error: **mark that proxy unhealthy, swap to another proxy, keep the
   account** (do not rotate the account for a proxy fault).
4. Proxy **health persisted** to config (survives restart).
5. Per-account `proxyURL` stays as an **override**: when set and healthy it
   wins; otherwise the account draws from the pool. Import keeps its per-account
   assign and also gains "add to pool".

## Decisions locked (from user)

- Failover: swap proxy, keep account.
- Pool: shared, pick-per-request.
- Health: persisted to config (durable).
- Coexistence: pool is the fallback; per-account override is preserved.

## Model

### Config schema

New type + field on `Config`:

```go
type PooledProxy struct {
    URL          string `json:"url"`                    // full scheme://[user:pass@]host:port
    Healthy      bool   `json:"healthy"`                // false after a failure until cooldown elapses
    FailCount    int    `json:"failCount,omitempty"`    // consecutive failures
    LastFailAt   int64  `json:"lastFailAt,omitempty"`   // unix seconds
    LastOKAt     int64  `json:"lastOkAt,omitempty"`     // unix seconds
    DisabledPermanent bool `json:"disabledPermanent,omitempty"` // operator-disabled, never auto-picked
}

// on Config:
ProxyPool []PooledProxy `json:"proxyPool,omitempty"`
```

Getters/setters in `config/config.go`, each calling `Save()`:
- `GetProxyPool() []PooledProxy`
- `AddProxyToPool(url string) error` (dedupe by URL; default Healthy=true)
- `RemoveProxyFromPool(url string) error`
- `MarkProxyUnhealthy(url string) error` (FailCount++, LastFailAt=now, Healthy=false)
- `MarkProxyHealthy(url string) error` (reset FailCount, Healthy=true, LastOKAt=now)
- `SetProxyPoolDisabled(url string, disabled bool) error`

Health writes happen off the request hot path infrequently (only on failure /
recovery), so a direct `Save()` is acceptable — no `markDirtyLocked` needed.

### Cooldown / recovery

A proxy marked unhealthy is skipped until `proxyUnhealthyCooldown` (const, e.g.
`5 * time.Minute`) elapses since `LastFailAt`; after that it's eligible again
(half-open). `DisabledPermanent` proxies are never auto-picked regardless of
cooldown. This mirrors the account-cooldown idea but persisted.

## Selection

New choke point in `proxy/kiro.go`, replacing direct
`ResolveAccountProxyURLStrict` calls on the request path:

```go
// SelectProxyForAccount returns the proxy URL to use for this request and an
// opaque token identifying the chosen pool entry (empty if not from the pool),
// so the caller can report success/failure back for health tracking.
func SelectProxyForAccount(account *config.Account) (proxyURL string, poolKey string, err error)
```

Order:
1. **Account override:** `account.ProxyURL != ""` → return it (poolKey="").
   (An explicit per-account proxy is honored as-is; it is not health-tracked in
   the pool because it isn't a pool member. If it fails it flows through the
   existing account-level path.)
2. **Pool:** pick a healthy, non-disabled, non-cooled-down entry. Selection is
   **round-robin** across eligible entries (a package-level atomic counter), so
   load spreads per request. Return its URL + URL as poolKey.
3. **Global proxy:** `config.GetProxyURL()` if set (poolKey="").
4. **Nothing + RequireProxy on** → error containing `"require-proxy"`.
5. **Nothing + RequireProxy off** → `("", "", nil)` (direct).

`ResolveAccountProxyURLStrict` stays for non-request callers; the request path
moves to `SelectProxyForAccount`.

## Failover on the request path

The proxy swap lives **inside `CallKiroAPI`**, not in every handler's
account-retry loop — localizes the change and keeps the pool logic in one file.

```
proxyAttempts := 0
for {
    proxyURL, poolKey, err := SelectProxyForAccount(account)
    if err != nil { return err }        // require-proxy: no proxy → account failover as today
    client := GetClientForProxy(proxyURL)
    // ... existing endpoint loop using client ...
    // classify the transport error:
    if isProxyErrorMessage(transportErr) && poolKey != "" && proxyAttempts < maxProxySwapAttempts {
        config.MarkProxyUnhealthy(poolKey)
        proxyAttempts++
        continue                        // swap proxy, SAME account
    }
    if success && poolKey != "" { config.MarkProxyHealthy(poolKey) }
    break
}
```

- `maxProxySwapAttempts` const (e.g. 3). If the pool can't yield a working
  proxy in N tries, return the proxy error → the existing account-level
  failover (Task 5) takes over as a last resort.
- Only **transport/dial/proxy** errors trigger a swap. HTTP 4xx/5xx from
  upstream are NOT proxy faults and must not mark a proxy unhealthy.
- The REST/background path (`restClientForAccount`) gets the same treatment via
  a small helper that selects + swaps on proxy error, so background refresh also
  benefits (bounded, e.g. 2 tries, no user waiting).

## UI

In the merged Proxy card:
- **Pool list:** render `ProxyPool` with masked `scheme://host:port`, health dot
  (green healthy / red unhealthy / grey disabled), fail count, and
  add/remove/enable-disable controls. New admin endpoints:
  - `GET /admin/api/proxy/pool` → list
  - `POST /admin/api/proxy/pool` → add `{url}` (or bulk)
  - `DELETE /admin/api/proxy/pool` → remove `{url}`
  - `POST /admin/api/proxy/pool/toggle` → `{url, disabled}`
- **Import integration:** the existing Import textarea gets a mode toggle:
  "Assign to accounts" (current behavior) vs "Add to shared pool" (new).
- The existing per-account badge + applied-proxy list stay; the applied list now
  also reflects pool-sourced routing where an account has no override.

## Files touched

- `config/config.go` — `PooledProxy` type, `ProxyPool` field, getters/setters,
  cooldown const; bump `Version` + `version.json`.
- `proxy/kiro.go` — `SelectProxyForAccount`, proxy-swap loop in `CallKiroAPI`,
  pool-aware REST helper.
- `proxy/kiro_api.go`, `proxy/kiro_overage.go` — route through the pool-aware
  REST helper (already centralized in `restClientForAccount`).
- `proxy/handler.go` — pool admin endpoints; extend `/proxy` payload if needed.
- `proxy/proxy_import.go` — "add to pool" mode.
- `web/index.html`, `web/app.js`, `web/styles.css`, `web/locales/*.json` — pool
  list UI + import mode toggle + i18n.

## Testing

- Unit: `SelectProxyForAccount` — override wins; pool round-robin skips
  unhealthy/disabled/cooled-down; global fallback; require-proxy error.
- Unit: `MarkProxyUnhealthy`/`MarkProxyHealthy` round-trip through config
  (persist + reload).
- Unit: cooldown eligibility (unhealthy within cooldown skipped; after elapsed,
  half-open eligible).
- Unit: `isProxyErrorMessage` already covers the swap trigger; add a test that a
  non-proxy HTTP error does NOT mark a proxy unhealthy.
- Manual: pool of 2 proxies, kill one, confirm requests swap to the other on the
  SAME account, the dead one shows red in the UI, and recovers after cooldown.

## Open risk

`config.Save()` on every proxy failure under a burst of failures could write
frequently. Mitigation: only write on a health **state transition** (healthy→
unhealthy or unhealthy→healthy), not on every failed attempt — `FailCount`
increments in memory, `Save()` fires on the transition. If this proves too
chatty, fold into the existing 30s dirty-flush like the stats counters.
