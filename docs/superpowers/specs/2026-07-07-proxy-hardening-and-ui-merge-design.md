# Proxy hardening + UI merge — design

Date: 2026-07-07

## Problem

Two issues with the outbound-proxy feature:

1. **Requests leak the server's real IP.** Users report that after a token
   refresh an account "falls out of the proxy" and must be re-imported. Root
   cause found in code: the external-IdP refresh path does OIDC discovery and one
   login leg through a **non-proxied** HTTP client, even though the proxy-aware
   client is available. The upstream IdP/AWS sees an unexpected IP and
   invalidates the session.

2. **The admin UI splits proxy config across two disconnected cards**
   ("Outbound Proxy Settings" and "Import Proxies"). The user wants them merged
   into one visual card, without regressing behavior.

The user also wants: (a) never leak the real IP — prefer failing over to another
account, (b) a per-request log line showing which proxy each request used, and
(c) a proxy indicator on each account card.

## Code findings (verified)

Leak points (both use `externalIdpHTTPClient()`, which has no proxy):
- `auth/kiro_sso.go:746` — `discoverOIDCEndpoints` (runs during external_idp
  refresh when the token endpoint isn't cached).
- `auth/kiro_sso.go:697` — one external_idp login leg.

Correct (already proxy-aware via `GetClientForProxy(ResolveAccountProxyURL(account))`):
- stream call `proxy/kiro.go:423`
- REST calls `proxy/kiro_api.go` (151/182/220/463)
- overage `proxy/kiro_overage.go` (67/142)
- token POST refresh honors the passed client `auth/kiro_sso.go:390`

`ResolveAccountProxyURL` (`proxy/kiro.go:101`) is the single choke point for
outbound proxy resolution: account proxy → global proxy → "".

Account UI is a card layout (`web/app.js` `renderAccounts`, ~825), not a table —
the proxy indicator becomes a badge in the card.

## Section 1 — Plug the leak

Thread the proxy-aware client down into discovery and login legs.

- `discoverOIDCEndpoints(issuerURL)` → `discoverOIDCEndpoints(issuerURL, client *http.Client)`.
- `resolveExternalIdpTokenEndpoint(issuerURL)` → also takes `client`.
- `RefreshExternalIdpToken` passes its `httpClient` down to discovery.
- When `client == nil`, fall back to `externalIdpHTTPClient()` (unchanged
  behavior for callers that don't supply one).
- Preserve the SSRF protection: the `CheckRedirect` that returns
  `http.ErrUseLastResponse`. If the passed proxy-aware client has no
  `CheckRedirect`, set it before use (or wrap). The proxy clients built in
  `buildAuthTransport` don't set `CheckRedirect`, so this must be applied.

Small, root-cause fix. No new config.

## Section 2 — Require-proxy toggle + failover when a proxy dies

**Config:** add `RequireProxy bool` to `Config` with getter/setter in
`config/config.go` that call `Save()`. Bump `Version` and `version.json`.

**Enforcement at the single choke point:** change `ResolveAccountProxyURL` to
also return an error (or add a sibling `ResolveAccountProxyURLStrict`):
- account/global proxy present → return it.
- no proxy and `RequireProxy == true` → return an error
  `"require-proxy: no proxy configured for account"`. Callers treat it as an
  account failure (no request is sent — the real IP is never exposed).
- no proxy and `RequireProxy == false` → return "" (direct, current behavior).

**Failover classification:** add `isProxyErrorMessage(msg)` to
`account_failover.go` matching proxy/dial failures (`proxyconnect`,
`socks`, `dial tcp`, `require-proxy`, connection refused/timeout on the proxy
hop). Route it to `h.pool.RecordError(id, false)` → **cooldown this account, try
the next**. Keep string-matchers in sync per project convention.

**Dial timeout (speed tradeoff mitigation):** the stream client has a 5-minute
timeout (needed for long SSE). A dead/hung proxy would otherwise hang the whole
request for 5 minutes before failover. Add a short **dial/connect timeout**
(~10s) on the proxy transport via `DialContext` (`net.Dialer{Timeout: 10s}`),
independent of the 5-minute total. A hung proxy fails in ~10s and rotates;
valid streams still get the full 5 minutes. Worst case with
`maxAccountRetryAttempts=3` is ~30s, not ~15min.

**Behavior chosen by user:** when `RequireProxy=true` and *all* accounts lack a
proxy, every request fails — accepted ("fail rather than leak IP").

## Section 3 — Observability

**Per-request log line (info level):** in `CallKiroAPI`, after resolving the
proxy, log one line covering every flow (stream/nonstream/openai/responses):

```
[Route] ac=jo***@x.com model=claude-sonnet-4 proxy=socks5h://1.2.3.4:1080 ep=KiroIDE
```

- Proxy is masked (scheme+host+port, password hidden) reusing the
  `ParsedProxy.MaskedURL` masking idea; a no-proxy account logs `proxy=direct`
  so leaks stand out. Info level so it's always visible.

**Proxy badge on account card:** in `renderAccounts`, add a badge:
- own proxy → shows masked `scheme://host:port` (no credentials).
- using global → `global`.
- none while require-proxy is on → `no-proxy` warning style (red).

Backend already returns per-account `proxyURL`; mask credentials before render
(only scheme+host+port). Add i18n keys for the new labels.

## Section 4 — Merge the two proxy cards

Combine the "Outbound Proxy Settings" and "Import Proxies" cards into one card
in `web/index.html`, split by sub-headings (reuse the `font-semibold` heading
pattern already used in the prompt-filter card):

```
Proxy
  Public Base URL ....... [Save]
  -- Global Proxy --
  Type (none/socks5/http); host:port; user:pass; [Save]
  [x] Require proxy (block direct / real-IP exposure)
  -- Bulk Import & Assign --
  textarea (one proxy per line)
  [x] Auto-test  [ ] Dry-run  [Import]
  results per line
```

**Key constraint — no logic regression:** keep every element ID unchanged
(`proxyType`, `proxyHost`, `proxyPort`, `proxyUsername`, `proxyPassword`,
`saveProxyBtn`, `publicBaseURL`, `savePublicBaseURLBtn`, `proxyImportList`,
`proxyImportAutoTest`, `proxyImportDryRun`, `proxyImportBtn`,
`proxyImportResults`). This is a pure markup/CSS merge; existing JS handlers and
i18n bindings are untouched. Only new JS: the require-proxy toggle
(load/save via a small admin endpoint or fold into the existing `/proxy`
GET/POST payload) and the account-card proxy badge.

**Require-proxy wiring:** extend the existing `/admin/api/proxy` GET/POST
(`handler.go` ~2259/2261) to carry `requireProxy` alongside `proxyURL`, so no
new endpoint is needed.

## Files touched

- `auth/kiro_sso.go` — thread client into discovery + login legs (Section 1).
- `config/config.go` — `RequireProxy` field + getter/setter; bump `Version`.
- `version.json` — bump.
- `proxy/kiro.go` — `ResolveAccountProxyURL` strict variant; dial timeout in
  `buildKiroTransport`; `[Route]` log line in `CallKiroAPI`.
- `proxy/account_failover.go` — `isProxyErrorMessage` matcher + routing.
- `proxy/handler.go` — `/admin/api/proxy` carries `requireProxy`.
- `web/index.html` — merge cards, add require-proxy checkbox.
- `web/app.js` — load/save require-proxy; proxy badge in `renderAccounts`.
- `web/locales/*.json` — new i18n keys.

## Testing

- Unit: `RefreshExternalIdpToken` with a stubbed discovery uses the passed
  (proxy) client, not a direct one (extend `auth/kiro_sso_test.go`,
  `testhooks.go`).
- Unit: `ResolveAccountProxyURL` strict returns error when require-proxy on and
  no proxy; returns proxy otherwise.
- Unit: `isProxyErrorMessage` matches representative dial/proxy errors.
- Unit: dial timeout is set on the proxy transport (extend `proxy/kiro_test.go`
  which already asserts transport proxy).
- Manual: run app, set a bad proxy, confirm request rotates account within
  ~10s and never goes direct; confirm `[Route]` log shows the proxy; confirm
  merged card saves global proxy, require-proxy, and import all work as before.
