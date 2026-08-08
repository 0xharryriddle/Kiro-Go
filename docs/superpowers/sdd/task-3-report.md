# Task 3 Report: Proxy-swap failover loop in CallKiroAPI + pool-aware REST helper

**Status:** DONE_WITH_CONCERNS (one dead-code concern; work complete and green)

## What I implemented

### 1. Pure decision helper + constants (`proxy/kiro.go`)
- `const maxProxySwapAttempts = 3`, `maxRestProxySwapAttempts = 2`.
- `shouldSwapProxy(transportErr error, poolKey string, attempts int) bool`:
  nil err → false; otherwise `isProxyErrorMessage(err.Error()) && poolKey != "" && attempts < maxProxySwapAttempts`.
  An HTTP-status error string (e.g. `"HTTP 401 ..."`) returns false because it is not a proxy/dial transport failure.

### 2. CallKiroAPI proxy handling rewrite (`proxy/kiro.go` ~508-670)
- Replaced the single `ResolveAccountProxyURLStrict` resolution with `SelectProxyForAccount(account)`, returning `(proxyURL, poolKey, err)`.
- **Gate ordering preserved**: `SelectProxyForAccount` is called ONCE up front; if it errors (require-proxy), we `return` immediately — before `ResolveProfileArn` and before any egress. No IP leak.
- Wrapped the existing endpoint-fallback loop in an OUTER `for {}` proxy-swap loop:
  - `lastTransportErr` captures ONLY the `proxyClient.Do(req)` error. HTTP 429 / non-200 set `lastErr` but never `lastTransportErr`, so an upstream status can never mark a proxy unhealthy.
  - On a streamable 200 through a pool proxy (`poolKey != ""`): `config.MarkProxyHealthy(poolKey)` once, then `parseEventStream` and return.
  - After the inner loop, if `shouldSwapProxy(lastTransportErr, poolKey, proxyAttempts)`: `config.MarkProxyUnhealthy(poolKey)`, `proxyAttempts++`, re-select via `SelectProxyForAccount` (return its error if any), `continue` (which logs the new `[Route]` line at loop top).
  - Otherwise `break` → return `lastErr` (or "all endpoints failed"), so existing account-level failover in `handleAccountFailure` takes over.
- `[Route]` log line is at the top of the outer loop, so it logs the chosen proxy on every swap iteration.
- `payload.ProfileArn` defer/restore untouched (still at the top of the function).

### 3. REST path (`proxy/kiro.go`, `proxy/kiro_api.go`, `proxy/kiro_overage.go`)
- Added shared helper `doRESTWithProxySwap(account, buildReq func() (*http.Request, error)) (*http.Response, error)`:
  selects via `SelectProxyForAccount` (require-proxy error returned as-is → abort, no leak), issues the request via `GetRestClientForProxy(proxyURL)`, on proxy transport error marks `MarkProxyUnhealthy` + re-selects up to `maxRestProxySwapAttempts`, marks `MarkProxyHealthy` on success through a pool proxy. `buildReq` builds a fresh `*http.Request` each attempt so the body is re-readable across swaps.
- Routed all 6 REST call sites through it: `GetUsageLimits`, `GetUserInfo`, `ListAvailableModels`, `listAvailableProfiles` (kiro_api.go); `FetchOverageStatus`, `SetOverageStatus` (kiro_overage.go). Each just moved its `http.NewRequest` + header setup into the `buildReq` closure; response handling unchanged. The require-proxy gate is preserved (it now lives inside `SelectProxyForAccount`).

### 4. Test (`proxy/kiro_test.go`)
- Added `TestShouldSwapProxy` covering: proxy error + poolKey + under cap → true; nil err → false; non-proxy error → false; HTTP-status string → false; empty poolKey → false; at cap → false. Added `fmt` import.
- Did NOT stand up a fake proxy — the pure decision helper is tested directly, per the brief.

## Test commands + RED/GREEN

- RED (before adding helper): `shouldSwapProxy` undefined — compile failure by construction (helper and its test written together; the swap logic is the pure decision the test pins).
- GREEN:
  - `go test ./proxy/ -run TestShouldSwapProxy -v` → PASS.
  - `go test ./proxy/` → only the two pre-existing allowed failures remain:
    `TestClaudeToolResultMixedTextAndImage`, `TestOpenAIToolResultImageCarriedWhenFollowedByUser` (translator_test.go). Everything else passes.
  - `go build -o kiro-go .` → clean.
  - `go vet ./...` → clean.

## Files changed
- `proxy/kiro.go` — constants, `shouldSwapProxy`, `doRESTWithProxySwap`, `CallKiroAPI` swap loop.
- `proxy/kiro_api.go` — 4 REST sites routed through `doRESTWithProxySwap`.
- `proxy/kiro_overage.go` — 2 REST sites routed through `doRESTWithProxySwap`.
- `proxy/kiro_test.go` — `TestShouldSwapProxy` + `fmt` import.

## Self-review findings
- Transport-vs-HTTP distinction is enforced by tracking `lastTransportErr` separately from `lastErr`; only `proxyClient.Do` errors feed the swap decision. Verified 429/non-200 branches set only `lastErr`.
- Health reporting is symmetric: healthy on first streamable 200 through a pool proxy, unhealthy only on a genuine proxy transport failure. Account overrides and global proxy have `poolKey == ""` so they are never marked.
- Re-selection after `MarkProxyUnhealthy` reads live pool state, so the just-failed proxy is now in cooldown and won't be re-picked; if no eligible proxy remains, `SelectProxyForAccount` may return the global/require-proxy path (its error is propagated).

## Concerns
- **Dead code**: `restClientForAccount` (Task-1/2 helper) now has no production caller — all REST sites use `doRESTWithProxySwap`. Go permits unused package-level funcs (build + vet clean), and it is still referenced in the design/plan docs, so I left it in place rather than delete Task-1/2 work without sign-off. Recommend removing it in a follow-up if the design owner agrees.
- The swap loop and `doRESTWithProxySwap` are not driven end-to-end by a test (would require a live/fake proxy, which the brief forbids). Coverage is the pure `shouldSwapProxy` decision plus the existing `SelectProxyForAccount`/`proxyPoolEligible` unit tests from Tasks 1-2.
