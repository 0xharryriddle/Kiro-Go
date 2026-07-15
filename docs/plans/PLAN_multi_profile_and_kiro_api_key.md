# Plan — Multi-profile support + upstream Kiro API-key authentication

Status: IMPLEMENTED — Phases 1–4 complete and validated; Phase 5 intentionally deferred
Owner: 0xharryriddle
Date: 2026-07-14
Target: current fork `0xharryriddle/Kiro-Go@8463cf1`

Phase 1 checkpoint (2026-07-14):
- Implemented the upstream credential abstraction, legacy key-account normalization, API-key header contract, key-bound profile soft-skip, region-pin dispatch exception, refresh bypass, readiness gates, and non-banning failover behavior.
- Added focused config/proxy tests for helper semantics, migration, header shaping, profile skipping, region validation, and refresh bypass.
- Validated with `gofmt`, `go build ./...`, `go vet ./...`, `go test ./config/ ./pool/ ./auth/ ./proxy/`, `node --check web/app.js`, locale-key symmetry, and `git diff --check`.

Phase 2 checkpoint (2026-07-14):
- Implemented TTL-bound, consume-once Kiro API-key probe/commit onboarding with strict secret validation, sanitized responses, successful-region enforcement, and atomic identity+region deduplication.
- Added admin UI onboarding, masked ordinary account surfaces, explicit secret-bearing full/export behavior, and live-validated import/export round trips.
- Added focused tests for probe expiry and concurrency, duplicate identity handling, response secrecy, import/export behavior, and API-key lifecycle invariants.

Phase 3 checkpoint (2026-07-14):
- Added `ProfilePinned` and atomic profile/region selection while protecting manual selections from stale whole-row writes and automatic self-heal.
- Implemented deterministic all-region discovery with ARN deduplication and partial-region warnings, plus authenticated discover/select/auto endpoints with fresh validation.
- Added the admin profile switcher, automatic-selection restoration, model-cache invalidation/reload, usability badges, warnings, and symmetric English/Chinese locale strings.

Phase 4 checkpoint (2026-07-14):
- Implemented hosted-SSO zero/one/multiple-profile outcomes with absolute credential expiry, TTL-bound parked choices, offered-ARN validation, consume-once finalization, and cancellation cleanup.
- Made finalization transactional: persistence failures retain the original parked credential and deadline for retry, while successful persistence consumes it exactly once.
- Serialized poll completion with overlapping polls and cancellation so no request can observe the auth-session-to-choice handoff gap or miss a newly parked credential.
- Added regression coverage for repeated/overlapping polls, poll-vs-cancel handoff, invalid and repeated finalization, persistence retry, and in-memory rollback after durable config write failure.

Phase 5 decision (2026-07-14):
- Deferred simultaneous multi-region route slots. Implementing them safely requires a normalized credential entity so one long-lived secret and one quota identity are not duplicated across account rows.
- Existing profile switching and one selected region per Kiro API-key account satisfy the requested behavior without secret duplication or quota distortion.

Final validation checkpoint (2026-07-14):
- Passed `gofmt`, `go build ./...`, `go vet ./...`, `go test ./config/ ./pool/ ./auth/ ./proxy/`, full targeted-package race tests, `node --check web/app.js`, locale key/type symmetry (778 leaves), and `git diff --check`.
- No commit or push was performed.

References pinned and audited:
- `anht3k52/kirogo-api@e13bd4e` (key origin commit `f3cde5212801a2b8752b1e4b6896b050cdf37d72`)
- `zsecducna/Kiro-Go@466a8ef` (multi-profile `5095a2a`, key auth `b7b9c33`, key onboarding/profile fix `7721e2c`, multi-region key probe `15d80f6`, identity dedup `02e2190`, model-refresh fix `4afdb79`)

---

## 0. Terminology (do not conflate these)

1. **Downstream proxy API key**: an `sk-...` key created by kiro-go to authenticate clients calling this proxy. Already implemented in `config/apikeys.go` and `proxy/auth.go`, hashed at rest.
2. **Upstream Kiro API key**: a Kiro-issued `ksk_...` credential used by kiro-go to authenticate to Kiro/Amazon Q. This feature. It must remain recoverable because it is sent upstream; hashing alone cannot replace encryption.
3. **Profile**: a CodeWhisperer/Kiro profile ARN. One OAuth credential may discover multiple profiles, including multiple regions. API-key credentials may be key-scoped and expose no listable profile ARN.
4. **Auth region**: `Account.Region`, used for OAuth/OIDC refresh.
5. **Data-plane region**: derived from selected profile ARN or `RegionOverride`; used for Kiro/Q calls. The two region concepts must stay separate.

---

## 1. Research findings

### 1.1 `anht3k52/kirogo-api`

Commit `f3cde52` implements the minimal key-auth model:
- `Account.KiroApiKey`, `AuthMethod="api_key"`, `IsApiKeyCredential()`.
- Header: `Authorization: Bearer <ksk_...>` plus `tokentype: API_KEY`.
- API-key accounts never refresh; `ExpiresAt=0`.
- UI/import paths create API-key accounts.
- It mirrors `KiroApiKey` into `AccessToken` for pool compatibility.
- It also introduced an independent auth/API-region refactor.

Reusable idea: header contract and refresh lifecycle.
Do **not** port directly: secret duplication (`KiroApiKey` and `AccessToken`), region URL construction that can create unsupported `codewhisperer.<region>` hosts, and weaker current-profile safety than this fork.

### 1.2 `zsecducna/Kiro-Go`

#### Multi-profile (`5095a2a`, `4afdb79`)
- Adds `DiscoverKiroProfiles`, probing every candidate region and returning all deduplicated ARNs.
- Existing account endpoints: `GET/POST /accounts/{id}/kiro-profiles`.
- Fresh external-IdP login: 0 profiles → lazy resolution, 1 → auto-select, 2+ → park exchanged credentials for 5 minutes and require an operator choice.
- Pending credentials use a mutex, timer, identity-checked expiry callback, and original absolute token expiry.
- POST switch validates the requested ARN against a fresh discovery before persisting.
- Model list is refreshed after switching.

Reusable ideas: fresh-discovery validation, TTL-bound pending SSO choice, identity-checked timer, absolute token expiry, model-cache refresh.
Do **not** port directly: direct `ProfileArn` writes do not reconcile this fork's hard `RegionOverride`, no explicit distinction between auto-cached and manually pinned profiles, and existing self-heal may later replace a manual selection.

#### Kiro API key (`b7b9c33`, `7721e2c`, `15d80f6`, `02e2190`)
- Uses the same upstream header contract.
- Skips profile-ARN resolution because Kiro keys are key-bound and may have no listable profile.
- Admin onboarding probes regions and later adds one pool account row per working region.
- Deduplicates by underlying `UserId + region`, falling back to key identity.
- Persists usage/subscription metadata from the probe.
- Avoids auto-banning throwaway/API-key accounts on best-effort usage refresh failures.

Reusable ideas: strict `ksk_` validation, region probe before persist, profile-resolution soft skip, identity dedup, metadata warm-up.
Do **not** port directly: duplicates the long-lived key into `AccessToken`; one account row per region duplicates the same secret and can distort routing/quota accounting; duplicate identity handling silently returns an existing account instead of explicitly resolving credential replacement; raw secret comparisons are used in places.

### 1.3 Current fork (`8463cf1`)

Already stronger than references in several areas:
- Multi-profile low-level primitive already exists: `listAllAvailableProfilesInRegion` and plan-aware `resolveProfileArnAcrossRegions`.
- Profile selection prefers a usable/in-plan profile and self-heals poisoned profile caches.
- Hard `RegionOverride` enforces host/ARN consistency and fail-closed generation.
- `acceptRefreshedProfileArn` prevents refresh paths from repoisoning a region pin.
- Model cache clear primitive exists.
- Downstream keys are hashed at rest and response cache is isolated by downstream key.

Gaps:
- No upstream credential abstraction; several gates equate usable credential with `AccessToken != ""`.
- Header construction only supports OAuth and external IdP.
- `ResolveProfileArn` probes every request for key-scoped credentials unless explicitly skipped.
- No public all-profile discovery/switch API or UI.
- Current region fail-closed check rejects an API-key account with no ARN, even though key-scoped dispatch is valid.
- No `ProfilePinned` distinction: a manual profile selection could be overwritten by self-heal.
- Normal account JSON/list/export/import/full-account surfaces need deliberate secret behavior.

---

## 2. Non-negotiable invariants

1. **One secret, one field**: upstream Kiro key lives only in `Account.KiroApiKey`; never mirror into `AccessToken`.
2. **No cleartext in ordinary APIs/logs**: account list exposes only credential type, `hasToken`, and a mask. No request/audit/error log may include the key. Explicit admin full/export/config backup are already sensitive channels and must be documented/tested.
3. **OAuth unchanged by default**: accounts with no Kiro key behave byte-for-byte as today.
4. **No refresh for Kiro keys**: `auth.RefreshToken` must never be called for `api_key` accounts.
5. **Key-bound profile semantics**: Kiro-key accounts may dispatch without `ProfileArn`; selected data-plane region still controls the host.
6. **Manual profile means pinned**: self-heal and refresh must not silently replace a manually selected profile.
7. **Profile and region are atomic**: selecting profile ARN X atomically aligns `RegionOverride` to X's region. Returning to auto clears the manual profile pin and the aligned override.
8. **Fresh validation**: a switch request may select only an ARN returned by a fresh discovery for that credential.
9. **No duplicate capacity**: same underlying Kiro identity + same selected region must not create multiple pool slots silently.
10. **In-flight semantics**: requests already holding an old account copy may finish; after persistence + pool reload, new requests use the new profile/credential.

---

## 3. Data model and helpers

### 3.1 `config.Account`

Add:
```go
KiroApiKey   string `json:"kiroApiKey,omitempty"`   // upstream Kiro-issued ksk_ credential
ProfilePinned bool  `json:"profilePinned,omitempty"` // ProfileArn is operator-selected, not cache
```

Do not add a second API-key token copy. Keep `AccessToken` OAuth-only.

Add helpers:
```go
func (a *Account) IsKiroAPIKeyCredential() bool
func (a *Account) HasUpstreamCredential() bool
func (a *Account) UpstreamBearerToken() string
func (a *Account) CanRefreshUpstreamCredential() bool
func (a *Account) CredentialKind() string // oauth | external_idp | api_key | none
```

Strict semantics:
- `IsKiroAPIKeyCredential` is true only for normalized `AuthMethod == "api_key"`; creation/import reject contradictory key + OAuth method.
- `HasUpstreamCredential`: nonempty Kiro key for API-key accounts; nonempty `AccessToken` otherwise.
- `UpstreamBearerToken`: key for API-key accounts, access token otherwise.
- `CanRefresh...`: false for API-key accounts; otherwise requires refresh material.

### 3.2 Load migration/normalization

On config load:
- If `KiroApiKey` is present and auth method empty/legacy `apikey`, normalize to `api_key`.
- If `AuthMethod=api_key` but key empty, leave account disabled and log a redacted warning; do not fall back to `AccessToken` silently.
- If a reference-derived config has both identical `KiroApiKey` and `AccessToken`, clear the duplicated `AccessToken` and persist once.
- Never infer API-key mode merely from an arbitrary `AccessToken` prefix.

### 3.3 Atomic profile selection

Add a config-layer method that mutates only profile fields under one lock:
```go
SelectAccountProfile(id, arn, region string, pinned bool) error
```
Manual select:
- set `ProfileArn=arn`
- set `ProfilePinned=true`
- set `RegionOverride=regionFromProfileArn(arn)`

Auto select:
- clear `ProfilePinned`
- clear cached `ProfileArn`
- clear `RegionOverride`

This avoids whole-struct lost-update races and makes the profile/host invariant persistent atomically.

---

## 4. Phased implementation

### Phase 1 — Upstream credential foundation (start immediately)

Files: `config/config.go`, `proxy/kiro_headers.go`, `proxy/kiro_api.go`, `proxy/kiro.go`, `proxy/handler.go`, `proxy/admin_usage_audit.go`, tests.

1. Add `KiroApiKey` and credential helper methods.
2. Header builder uses `UpstreamBearerToken`; for API-key accounts add `TokenType: API_KEY`. Preserve `TokenType: EXTERNAL_IDP` for external IdP.
3. `ResolveProfileArn` returns a recognized soft-skip for API-key accounts before probing.
4. Region hard-pin generation check permits empty ARN only for API-key accounts; mismatched nonempty ARN remains forbidden.
5. `ensureValidToken` is a no-op for API-key accounts.
6. Replace readiness gates (`AccessToken != ""`) with `HasUpstreamCredential` in background refresh, model warm-up/re-enable, usage audit, and account `hasToken` response.
7. Background/manual refresh never invokes OAuth refresh for API-key accounts.
8. Tests: OAuth headers unchanged; external IdP unchanged; API-key header; empty key emits no auth; profile skip; region override + empty ARN accepted only for API-key accounts; readiness helper table; refresh skipped.

Phase 1 deliberately does not yet expose a UI/add endpoint; it creates a safe shared foundation first.

### Phase 2 — Secure Kiro API-key onboarding

Backend API under existing admin auth:
- `POST /admin/api/auth/kiro-api-key/probe`
- `POST /admin/api/auth/kiro-api-key/commit`

Probe request: `{kiroApiKey, regions?}`.
- Strict `ksk_` prefix, length bound, no whitespace/control chars.
- Probe candidate regions (`account/auth region`, current `KIRO_PROFILE_REGIONS`, defaults) with a throwaway API-key account.
- Return only `{probeId, expiresAt, regions:[{region, usable, email?, userId?, subscription?, errorCode?}]}`; never echo key or raw upstream body.
- Park the key in memory for 5 minutes using the identity-checked timer pattern from `5095a2a`.

Commit request: `{probeId, selectedRegion, nickname?, enabled?}`.
- Region must be one of the successful probe results.
- Consume pending key exactly once.
- Dedup by `UserId + selectedRegion`; return HTTP 409 with existing account metadata instead of silently pretending the new key was installed.
- Persist one account row and one secret, with `RegionOverride=selectedRegion`, `ProfileArn=""`, `ProfilePinned=false`, `ExpiresAt=0`.
- Persist probe metadata and warm model cache.

UI:
- Add “Kiro API Key” credential method with password input.
- Probe first; if one working region, preselect; if multiple, require selection.
- Never place key into DOM after submit; clear input immediately.
- List/detail show “Kiro API Key”, masked suffix, selected region, never cleartext.

Import/export:
- Credentials import accepts `kiroApiKey` only with `authMethod=api_key`; validates and preferably routes through probe/commit.
- Explicit secret-inclusive export includes it for round-trip; ordinary account list does not.
- Config backups remain secret-bearing and must retain existing filesystem protections.

### Phase 3 — Existing-account multi-profile discovery and switching

Add:
```go
type KiroProfile struct {
    Arn string
    Region string
    Usable bool
    Current bool
}
```

Discovery must ignore a current `RegionOverride` when searching for alternatives, but candidate regions include, in order:
1. current profile ARN region
2. current region override
3. account auth region
4. configured/default fallback regions

New endpoints:
- `GET /admin/api/accounts/{id}/kiro-profiles`
- `POST /admin/api/accounts/{id}/kiro-profiles` body `{profileArn}`
- `POST /admin/api/accounts/{id}/kiro-profiles/auto`

GET:
- use freshest pool tokens
- OAuth/external-IdP accounts only initially; API-key accounts return mode `key_bound` and no ARN picker
- probe all candidates; return profiles plus per-region warnings so partial discovery is not misrepresented as complete

POST select:
- reject API-key accounts (`key_bound`)
- fresh discovery; requested ARN must be present
- validate ARN structure and region
- atomically set profile + pin + aligned region override
- clear profile cooldown and old model cache
- reload pool
- synchronously refresh model list; report `modelsRefreshed` without undoing a successful switch

POST auto:
- atomically clear manual pin/profile/region override
- clear cooldown/model cache, reload, resolve/refresh

Self-heal changes:
- when `ProfilePinned`, `reresolveProfileArn` must not replace the selected ARN
- profile/plan failure becomes a soft account failure with an operator-action message, not automatic profile switching
- auto-mode behavior remains unchanged

UI:
- “Switch profile” on eligible accounts
- list region + full ARN + current/manual badge + usability
- explicit “Automatic selection” option
- warning that selecting a profile aligns the data-plane region and overrides the standalone region pin

### Phase 4 — Multi-profile choice during hosted SSO

Adapt the robust parts of `5095a2a`:
- external-IdP login discovers eagerly before account creation
- 0 → current lazy fallback
- 1 → set profile (auto, not manual pin unless product decision says otherwise)
- 2+ → park exchanged credential for 5 minutes and require choice
- store absolute access-token expiry at exchange time
- identity-check timer callback; original deadline survives invalid retries
- cancel drops pending credential
- selected ARN must be from offered list

Because manual selection is explicit in this flow, finalize with `ProfilePinned=true` and aligned `RegionOverride`.

### Phase 5 — Optional simultaneous multi-region route slots

Do not duplicate a `KiroApiKey` across account rows as the reference does. If simultaneous routing through multiple regions is required, first normalize storage:
- `UpstreamCredential` entity stores the secret once
- route/profile slots reference `credentialId`
- migrate existing account-inline credentials

This is intentionally deferred; profile switching and single-selected-region key accounts satisfy the requested feature without secret duplication or quota distortion.

---

## 5. Security and failure behavior

- Never log request bodies containing `kiroApiKey`; sanitize `ksk_...` in generic error/log redaction.
- Constant-time compare or SHA-256 fingerprint for key dedup; do not expose fingerprint in public APIs.
- Probe errors return categorized codes (`unauthorized`, `region_unavailable`, `network`, `upstream`) and sanitized messages.
- Throwaway probe accounts must never mutate config or trigger ban writes.
- API-key 401/403 should cool down the account and surface status; do not permanently ban from one best-effort metadata fetch. Repeated data-plane auth failures may disable only under an explicit threshold policy.
- Existing downstream `sk-...` key auth remains isolated and unchanged.
- Explicit full-account/export/config-backup endpoints are sensitive; normal account list gets only `kiroApiKeyMask` and `hasToken`.

---

## 6. Tests

### Unit
- credential helper matrix
- header matrix: OAuth, external IdP, API key, empty key, contradictory state
- config migration clears duplicated AccessToken
- API-key profile soft skip
- region hard pin accepts key-bound empty ARN but rejects OAuth empty/mismatch
- profile discovery dedup/order/partial errors
- atomic select/auto and region alignment
- pinned profile cannot self-heal away

### Handler/API
- add/probe missing, bad prefix, bad type, unauthorized admin
- key never appears in probe response/logs/account list
- probe TTL, consume-once, invalid-region selection, concurrent commit
- identity+region duplicate returns 409
- existing profile GET/POST validates fresh discovery
- API-key account profile endpoint returns `key_bound`
- switch clears stale model cache and reloads pool
- import/export round-trip

### Regression
- OAuth refresh/header/profile resolver unchanged
- external-IdP `TokenType` unchanged
- existing region override tests
- account failover, model allow-list, quota-aware routing, overage, usage audit
- downstream API-key hash/usage/rate-limit suite

### Live
- OAuth account unchanged
- Kiro key valid in one region: probe → commit → models/usage/generation
- key invalid: no account persisted
- multi-profile OAuth account: discover → select → host/ARN match → auto restore
- profile-less API-key generation succeeds with selected regional host
- secrets absent from logs and ordinary admin account response

---

## 7. Rollout and rollback

1. Ship Phase 1 helpers behind no UI/API exposure; validate full suite.
2. Ship Phase 2 onboarding with pending probes; monitor sanitized error counts.
3. Ship Phase 3 switcher to existing accounts.
4. Ship Phase 4 SSO picker.

Rollback:
- Old binaries ignore new JSON fields (`kiroApiKey`, `profilePinned`) but cannot serve API-key accounts. Before rollback, disable API-key accounts or retain Phase 1 binary.
- Profile selection writes existing `ProfileArn`/`RegionOverride`, so OAuth accounts remain routable by the pre-feature current fork; `profilePinned` is ignored but harmless.
- Keep config backups before migrations.

---

## 8. Files expected

Phase 1: `config/config.go`, `proxy/kiro_headers.go`, `proxy/kiro_api.go`, `proxy/kiro.go`, `proxy/handler.go`, `proxy/admin_usage_audit.go`, new `proxy/kiro_upstream_apikey_test.go`, config tests.

Later phases: `proxy/handler.go` (or split `proxy/kiro_profiles_admin.go`, `proxy/kiro_apikey_admin.go`), `proxy/kiro_api.go`, `config/config.go`, `pool/account.go`, `web/app.js`, `web/locales/en.json`, `web/locales/zh.json`, focused tests.

Prefer splitting new handlers into focused files rather than growing `handler.go` further.
