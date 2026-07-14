# Plan — API-Key Feature: Peer-Reviewed E2E Hardening Roadmap

Status: IN PROGRESS (P0 slice shipped; remaining phases pending)
Owner: 0xharryriddle
Scope: Kiro-Go proxy only. No hermes-agent edits.
Date: 2026-07-14

---

## 0. TL;DR — the feature is NOT missing, it is fragmented + insecure

Five independent audits (5 subagents, cx/gpt-5.6-sol) converged: Kiro-Go already
has a working multi-API-key subsystem. What made it feel "missing" is a mix of
**discoverability** (management is buried in Settings, while the prominent "API"
tab is read-only docs) and **security/correctness gaps** that undermine trust in
it. This plan fixes both, foundation-first.

### What already exists (verified)
- `config/apikeys.go` — full CRUD, usage counters, per-model breakdown, masking,
  generation (`sk-` + 32 random bytes), reset, legacy-key migration flag.
- `proxy/auth.go` — `authenticate()` master switch (`RequireApiKey`), managed-key
  path, legacy single-key fallback, fail-closed when switch on but nothing set.
  Accepts `Authorization: Bearer …` and `X-Api-Key`.
- `proxy/rate_limiter.go` — in-process sliding-window RPM/TPM (F6).
- `proxy/admin_apikeys.go` — admin CRUD; masked in list/get/update; cleartext
  only on create.
- `web/index.html` + `web/app.js` — Settings panel: enforcement toggle, create/
  custom/enable/disable/edit-limits/reset/delete, one-time copy dialog, per-model
  usage. en/zh locales complete.
- Usage attribution wired through `apiKeyIDFromContext` →
  `recordSuccessForApiKey` on OpenAI/Claude/Responses, stream + non-stream.

---

## 1. Prioritized gap matrix (audit consensus, file:line verified)

| ID | Sev | Gap | Evidence |
|----|-----|-----|----------|
| G1 | P0  | Response cache crosses API-key tenants + cache hits bypass usage/TPM accounting | `response_cache.go:14,45`; `handler.go:1071,1993` |
| G2 | P0  | API keys + admin password stored PLAINTEXT in config and every rotating backup + config export | `config/config.go:157,160,199,469,487`; `handler.go` export |
| G3 | High| TPM never enforced at admission — `Admit(...,estTokens=0)` while TPM branch needs `estTokens>0` | `auth.go:91`; `rate_limiter.go:105` |
| G4 | High| Auth comparison not constant-time; linear plaintext scan | `apikeys.go:136` |
| G5 | Med | "Shown once" is UI convention, not storage truth (keys live in config/backups/export) | `admin_apikeys.go`; `config.go:160` |
| G6 | Med | Discoverability: key admin under Settings; read-only "API" tab is where users look | `index.html:106,110,398,639` |
| G7 | Low | No scopes / per-key model restrictions / expiry / rotation | schema `config/config.go` ApiKeyEntry |
| G8 | Low | RNG failure in key generation ignored (`rand.Read` err dropped) | `apikeys.go:225` |
| G9 | Low | No client quick-start / curl examples / live key-test in UI | `index.html` API tab |

---

## 2. Execution order (foundation-first; compatibility-preserving)

### Phase 0 — P0 correctness/isolation  ✅ SHIPPED (this change set)
- **G1 fixed.** `responseCacheKey(apiKeyID, endpoint, body)` now namespaces every
  cache entry by authenticated API-key identity → one tenant's cached response
  can never be served to another key. Empty identity (auth-off / legacy single
  key) is its own namespace = the same trust boundary those requests already
  share.
- **G1 accounting fixed.** Cache HITS now attribute the cached response's usage
  to the key via `recordSuccessForApiKey` (parsed by `usageFromCachedOpenAIBody`
  / `usageFromCachedClaudeBody`), so hits can no longer bypass token/credit
  quotas or the RPM/TPM windows.
- Tests: `TestResponseCacheKey_TenantIsolated`, `TestUsageFromCachedOpenAIBody`,
  `TestUsageFromCachedClaudeBody`, plus updated stable/namespace tests.
- Bundled (separate concern, same deploy): **assistant-prefill 400 fix** per
  `PLAN_assistant_prefill_400.md` — trailing-assistant arrays now fold into
  history + generate against a `Continue.` nudge instead of 400ing.

### Phase 1 — Hashed-at-rest secrets (G2, G4, G5)  ← NEXT
Compatibility-critical; ship atomically (schema + lookup + create + migration +
UI all together, else clients lock out).
- Add `KeyHash` (HMAC-SHA256 with a per-install secret) + `KeyPrefix` (first 8
  chars, non-secret, for display/identify) to `ApiKeyEntry`; stop persisting the
  raw `Key`.
- `FindApiKeyByValue` → compute HMAC of the presented key, `hmac.Equal` against
  stored hashes (constant-time), keyed by prefix to keep it O(1)-ish.
- One-way migration on load: for any entry still carrying plaintext `Key`,
  derive hash+prefix, blank the plaintext, set `Migrated=true`, persist once.
- Redact `Key`/`KeyHash`/`Password` from the config-export endpoint.
- Create returns the cleartext exactly once (unchanged UX); storage keeps only
  the hash. Update the "shown once" locale copy to be literally true.
- Backups: on the migrating save, the new backup is already redacted; document
  that pre-migration backups must be rotated/destroyed (operator runbook).
- Tests: migration idempotence, constant-time match, no-plaintext-in-marshalled
  config, RNG-failure aborts create (G8).

### Phase 2 — TPM reservation correctness (G3)
- `authenticate` (or the pre-dispatch admission point) passes the request's
  `estimatedInputTokens` into `Admit` so an already-over-budget key is rejected
  at admission, not after.
- Keep `RecordTokens` post-response folding of actuals.
- Tests: integration test that a key over TPM gets 429 at admission on the real
  `/v1/chat/completions` path (not just the limiter unit).

### Phase 3 — Discoverability + client quick-start (G6, G9)
- Surface API-key management on the prominent **API** tab (or a dedicated "API
  Keys" tab), not buried in Settings.
- Add a copy-paste quick-start (base URL, `Authorization: Bearer sk-…`, curl for
  OpenAI + Claude routes) and an in-UI "Test this key" button hitting a cheap
  endpoint.

### Phase 4 — Authorization richness (G7) [optional / later]
- Per-key model allow-list, expiry (`ExpiresAt`), scopes, rotate-in-place.

### Phase 5 — Release gates / ops
- Race tests on counters + limiter, fuzz auth/header/JSON, migration rollback
  drill, operator docs (TLS/reverse-proxy requirement, key lifecycle, cache
  behavior, backup handling, incident rotation).
- Gate: no plaintext keys in active config or new backups; deterministic
  concurrency tests; full route-policy E2E.

---

## 3. Compatibility & honesty notes
- Phase 1 is the only breaking-shaped change; done as a load-time auto-migration
  so existing keys keep working with zero operator action. Raw keys already
  handed to clients are unaffected (only the at-rest form changes).
- In-memory RPM/TPM remains single-instance (documented); no Redis introduced.
- Cache-hit accounting counts the cached response's own reported usage; this is
  a defensible "hits still cost quota" policy and closes the bypass. If a
  zero-cost-hit policy is ever desired, make it an explicit config toggle.

## 4. Verification gate (every phase)
`gofmt -l proxy/ config/` clean on touched files → `go build ./...` → `go vet` →
targeted tests → full `go test ./config ./proxy ./pool ./auth` → rebuild
container → `/healthz`+`/readyz` 200 → live smoke (create key, call `/v1/chat/
completions` with it, confirm 200 + usage increments; wrong key → 401).
