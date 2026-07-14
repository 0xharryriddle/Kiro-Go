# Plan — Per-account region switching (manual data-plane region override)

Status: PLAN (peer-review pass appended below; implementation to follow)
Owner: 0xharryriddle
Scope: Kiro-Go proxy only. No hermes-agent edits.
Date: 2026-07-14

---

## 0. What "region switching" means here (scoped decision)

Target: **per-account manual region override** — let an operator pin which AWS
data-plane region an account's Kiro/Q calls go to, from the admin UI, overriding
the region that is otherwise auto-derived from the profile ARN.

Explicitly OUT of scope for this slice (candidates for a follow-up):
- Global editable fallback-region list (currently `KIRO_PROFILE_REGIONS` env).
- Automatic region failover on region-specific errors/throttling.

This matches the established per-account control pattern (`ProxyURL`,
`ModelAllowList`, `Weight`) and is the most direct reading of the request.

---

## 1. How region is derived TODAY (source-verified)

Data-plane region resolution — `kiroRegionForProfile` (`proxy/kiro_api.go:40`):
```
1. regionFromProfileArn(payloadProfileArn)      // ARN passed for this request
2. regionFromProfileArn(account.ProfileArn)     // cached ARN  ← usually wins
3. account.Region                               // auth/OIDC region
4. "us-east-1"                                   // default
```
`regionalizeURLForRegion` (`kiro_api.go:77`) then rewrites the hardcoded
`q.us-east-1.*` / `codewhisperer.us-east-1.*` hosts to `q.{region}.amazonaws.com`
(no-op for us-east-1). Data-plane dispatch uses it at `kiro.go:397`.

Profile discovery — `resolveProfileArnAcrossRegions` (`kiro_api.go:470`) probes
`kiroProfileRegionCandidates` (`kiro_api.go:106`): the account region first, then
fallbacks (built-in `us-east-1,eu-central-1` or `KIRO_PROFILE_REGIONS`), but ONLY
when `shouldProbeFallbackRegions` is true (external_idp / Enterprise / no region).

Region is captured at login/import (`account.Region`) and shown READ-ONLY in the
detail modal (`web/app.js:1769`). `apiUpdateAccount` (`handler.go:3108`) does NOT
currently accept a region field.

### The load-bearing subtlety (why naive "edit account.Region" fails)

For any account that already has a cached `ProfileArn` (most do), step 2 of
`kiroRegionForProfile` wins — so editing `account.Region` alone is **silently
ignored** for data-plane routing. The ARN's embedded region dominates. Therefore
a real override must (a) take precedence over the ARN-derived region for
data-plane calls, AND (b) force the cached ARN to be re-resolved against the new
region, or the ARN and the target host will disagree and calls will 400/403.

---

## 2. Design — `RegionOverride` (data-plane only), with ARN re-resolution

### 2a. Config field (`config/config.go`)
Add to `Account`:
```go
// RegionOverride pins the AWS data-plane region for this account's Kiro/Q
// calls, overriding the region auto-derived from the profile ARN. Empty = no
// override (auto-derivation as before). This is the data-plane region only; the
// auth/OIDC region (Region) is unchanged, because they can legitimately differ.
RegionOverride string `json:"regionOverride,omitempty"`
```
Add a small helper:
```go
func (a *Account) EffectiveRegionOverride() string { return strings.TrimSpace(a.RegionOverride) }
```

### 2b. Region derivation respects the override FIRST (`proxy/kiro_api.go`)
`kiroRegionForProfile` gains a first-precedence check:
```go
func kiroRegionForProfile(account *config.Account, profileArn string) string {
    if account != nil {
        if ov := account.EffectiveRegionOverride(); ov != "" {
            return ov            // manual override wins over ARN/auth region
        }
    }
    // ... existing ARN → account.Region → us-east-1 ladder unchanged ...
}
```
This makes EVERY data-plane call (generate, getUsageLimits, GetUserInfo, models)
target the override region via the existing `regionalizeURL*` plumbing — no other
call-site edits needed.

### 2c. Profile discovery is pinned to the override (`proxy/kiro_api.go`)
`kiroProfileRegionCandidates`: when an override is set, probe ONLY that region
(the profile the override wants must live there):
```go
if ov := account.EffectiveRegionOverride(); ov != "" {
    return []string{ov}
}
```
Placed before the account-region/fallback logic. This guarantees
`resolveProfileArnAcrossRegions` finds (or fails to find) the profile in the
override region, and the resolved ARN then carries that region consistently.

### 2d. Changing the override forces ARN re-resolution (`proxy/handler.go`)
In `apiUpdateAccount`, accept `regionOverride` and, when it CHANGES, clear the
cached `ProfileArn` so the next call re-resolves against the new region:
```go
if v, ok := updates["regionOverride"].(string); ok {
    newOv := strings.TrimSpace(v)
    if newOv != strings.TrimSpace(existing.RegionOverride) {
        existing.RegionOverride = newOv
        existing.ProfileArn = ""   // force re-resolution in the (new) region
    }
}
```
`config.UpdateAccount` persists the whole struct (verified: it writes `*existing`),
so blanking `ProfileArn` sticks. `h.pool.Reload()` (already called at the end of
`apiUpdateAccount`) refreshes routing. On the next request `ensureRestProfileArn`
→ `resolveProfileArnAcrossRegions` re-discovers the ARN in the override region.

Validation: reject obviously malformed regions (basic shape check, e.g. matches
`^[a-z]{2}-[a-z]+-\d$`) to avoid a typo silently breaking an account; empty is
always allowed (clears the override).

### 2e. Optional immediate re-validation
After setting the override, trigger the same async model/profile refresh
`apiUpdateAccount` already does on enable (`fetchAndCacheAccountModels`) so the UI
reflects the new region's models without waiting for the next live request. Guard
it behind `existing.Enabled && existing.AccessToken != ""` exactly as the existing
re-enable path does.

### 2f. UI (`web/`)
- `web/app.js` `showDetail` (~1769): replace the read-only region line with an
  editable control. Two fields side by side:
  - "Auth region" (read-only, `a.region`) — informational.
  - "Data-plane region override" — an `<input>` (free text, since AWS regions are
    an open set) with a datalist of common regions
    (`us-east-1, eu-central-1, ap-southeast-2, ...`) and a Save button
    (`data-detail-action="saveRegionOverride"`).
- Dispatch case in the detail click handler (~3884) → `saveRegionOverride(id)`.
- `saveRegionOverride(id)`: read input, client-side shape check, `putAccount(id,
  { regionOverride: value }, t('detail.regionOverrideSaved'))`.
- `apiGetAccounts` response map (`handler.go:3033`): add `"regionOverride":
  a.RegionOverride` so the UI can show the current value.
- Show a small hint that clearing it restores automatic region detection, and
  that changing it re-resolves the profile (may take one request to settle).

### 2g. Locales (`web/locales/en.json`, `zh.json`)
Add (alpha-ordered under `detail.*`): `detail.regionOverride`,
`detail.regionOverrideHint`, `detail.regionOverrideSaved`,
`detail.regionOverrideInvalid`, `detail.authRegion`. Keep en/zh parity.

---

## 3. Files touched (exhaustive)

| File | Change |
|------|--------|
| `config/config.go` | `RegionOverride` field + `EffectiveRegionOverride()` helper |
| `proxy/kiro_api.go` | override-first in `kiroRegionForProfile`; pin `kiroProfileRegionCandidates` |
| `proxy/handler.go` | accept `regionOverride` in `apiUpdateAccount` (+ ARN reset on change); expose in `apiGetAccounts` |
| `proxy/kiro_region_test.go` | unit tests (§5) |
| `web/app.js` | editable override field + `saveRegionOverride` + dispatch |
| `web/index.html` | (only if a static datalist is added) |
| `web/locales/en.json`, `zh.json` | 5 keys, parity |

No changes to auth/OIDC region handling, streaming transport, or token accounting.

---

## 4. Compatibility & honesty notes

- Backward-compatible: `RegionOverride` empty = today's behavior exactly. All
  existing accounts are untouched (zero-value, `omitempty`).
- Auth region (`account.Region`) is deliberately NOT changed by the override —
  the codebase is explicit that auth and data-plane regions can differ, and
  RefreshToken/OIDC keys off `account.Region`. Conflating them could break token
  refresh. Override is strictly data-plane.
- Honest limit: an override to a region where the account has NO profile will
  fail profile resolution ("no available Kiro profile") — by design, because we
  pin discovery to that region. The UI hint must say so. This is correct
  fail-closed behavior, not a regression.
- The override interacts with `shouldProbeFallbackRegions`: when an override is
  set we bypass fallback probing entirely (single pinned region), which is the
  intended semantics of a manual pin.

---

## 5. Tests (`proxy/kiro_region_test.go`)

- `TestKiroRegionForProfileOverrideWins`: account with cached ARN in us-east-1 +
  `RegionOverride=eu-central-1` → `kiroRegionForProfile` returns `eu-central-1`
  (proves override beats the ARN-region precedence — the core subtlety).
- `TestKiroRegionForProfileNoOverrideUnchanged`: empty override → existing ladder
  (ARN → account.Region → us-east-1) is preserved byte-for-byte.
- `TestKiroProfileRegionCandidatesOverridePinsSingleRegion`: override set →
  candidates == `[override]` only, regardless of auth method / fallbacks / env.
- `TestRegionalizeURLForRegion*` (existing) stay green — the override rides the
  same primitive.
- Handler-level: `apiUpdateAccount` with a changed `regionOverride` blanks the
  cached `ProfileArn`; unchanged value does NOT blank it (avoid needless
  re-resolution). Add if a handler test harness exists; else cover via the
  config round-trip.

---

## 6. Verification gate

1. `gofmt -l` clean on touched files → `go build ./...` → `go vet ./...`.
2. `go test ./config/ ./pool/ ./auth/ ./proxy/` → all pass.
3. `node --check web/app.js`; JSON valid; en/zh locale key parity.
4. Rebuild container; `/healthz` + `/readyz` 200.
5. **Live**:
   - Pick an account; set `regionOverride=eu-central-1` via the UI/PUT.
   - Confirm `apiGetAccounts` reflects it and the cached ARN was blanked.
   - Fire a request on that account; confirm data-plane host is
     `q.eu-central-1.amazonaws.com` (add a temporary debug log or inspect via the
     diagnostics endpoint) and the profile re-resolved in that region.
   - Clear the override; confirm behavior reverts to auto-derivation.
6. Confirm an account with NO override is byte-for-byte unaffected.

---

## 7. Peer-review / debate log

TWO independent adversarial passes were run against the actual source: my own
self-review (7A) and an independent reviewer on cx/gpt-5.6-sol (7B). 7B found
FOUR High-severity flaws my self-review missed. Both verified against source
below; the consolidated verdict and the HARDENED design are in §8. **The
original §2 design is NOT safe to implement as-is — build §8 instead.**

### 7A. Self-review findings (verified against source)

### 7.1 CONFIRMED-CORRECT (verified file:line)

- C1. Single derivation chokepoint holds. Every data-plane call routes through
  `regionalizeURL`/`regionalizeURLForProfile` → `kiroRegionForProfile`. Verified
  sites: generate `kiro.go:397`; getUsageLimits `kiro_api.go:172`; GetUserInfo
  `kiro_api.go:202`; models path `kiro_api.go:256`; overage GET
  `kiro_overage.go:56`; overage SET `kiro_overage.go:135`. So overriding
  `kiroRegionForProfile` (§2b) catches ALL data-plane sites with no per-site edit.
- C2. The one direct `regionalizeURLForRegion` caller that bypasses
  `kiroRegionForProfile` is the discovery probe `kiro_api.go:681`
  (`listAvailableProfilesInRegion`) — correctly bypassed, and §2c pins it via
  `kiroProfileRegionCandidates`.
- C3. `config.UpdateAccount` persists the whole struct, so blanking `ProfileArn`
  in `apiUpdateAccount` sticks; `apiAddAccount` decodes the full struct so
  `regionOverride` is settable at add-time for free.
- C4. Pool carries the new field automatically: `pool.Reload()` copies whole
  `config.Account` structs, so `RegionOverride` and the blanked `ProfileArn` are
  reflected in routing after the existing `h.pool.Reload()` call.

### 7.2 FLAWS / RISKS

- F1 (MEDIUM) — validation regex is too strict. The proposed
  `^[a-z]{2}-[a-z]+-\d$` REJECTS valid AWS regions with multi-part middles:
  `us-gov-east-1`, `us-gov-west-1`, and future forms. It also rejects any
  two-digit index. FIX: use `^[a-z]{2}(-[a-z]+)+-\d+$` (or skip strict regex and
  just trim + lower + length-bound). Empty always allowed. Better: validate
  shape loosely and rely on the "no profile in that region → fail-closed" behavior
  rather than a brittle allow-pattern.
- F2 (LOW/MEDIUM) — profile-ARN resolution cooldown ignores region. When the
  override changes we blank `ProfileArn`, but `ResolveProfileArn`
  (`kiro_api.go:297`) first checks `isProfileArnResolutionSuppressed`, whose key
  (`profileArnCooldownKey`, `kiro_api.go:355`) is `provider\x00id` — NO region.
  So an account currently under a Builder-ID-"unsupported" cooldown will NOT
  re-probe after a region change until the cooldown expires. Narrow: suppression
  is only set for `isBuilderIDProfileUnsupportedError` (a genuine BuilderId-can't-
  do-profiles 403, which is region-independent anyway), so this is arguably
  correct. FIX (optional): on a region-override CHANGE, also delete the cooldown
  key so a manual switch always forces a fresh probe. Cheap, removes surprise.
- F3 (LOW) — `retryUsageWithReresolvedProfile`/`reresolveProfileArn`
  (`kiro_api.go:542,566`) self-heal writes `account.Region = regionFromProfileArn(newArn)`.
  With an override set, the discovered ARN is in the override region, so this
  writes the override region into `account.Region` too. Harmless (they converge),
  but note it: after first use, `account.Region` may equal the override. Not a bug;
  document it so it doesn't look like state corruption.
- F4 (LOW) — UI datalist is advisory only. AWS regions are an open set; the input
  must remain free-text (plan already says this). Ensure the client-side check is
  a warning, not a hard block, or it will collide with F1.

### 7.3 MISSED CALL SITES / EDGE CASES

- M1. `apiAddAccount` (`handler.go:3065`) decodes the full `config.Account`, so a
  region override supplied at creation is honored with zero extra code — but the
  ARN-reset-on-change logic in §2d lives only in `apiUpdateAccount`. That is
  correct (a brand-new account has no cached ARN to reset), so no action; note it
  for completeness.
- M2. Auth/login region inputs (`handler.go:3442/3514/3614`) are a DIFFERENT
  region concept (OIDC/SSO). The plan correctly leaves them untouched; confirm no
  UI wiring accidentally points the new override control at those flows.

### 7.4 Self-review verdict (SUPERSEDED by 7B — kept for the record)

My self-review concluded "CHANGES REQUIRED (minor)" with 3 small fixes (F1
regex, F2 cooldown, F3 self-heal comment). This was TOO OPTIMISTIC — it missed
the profile-ARN-acquisition bypass paths that the independent reviewer caught.
The real verdict is 7B below.

---

### 7B. Independent reviewer findings (cx/gpt-5.6-sol) — VERIFIED against source

The independent pass found FOUR High-severity flaws. I re-verified each against
the actual code before accepting:

- **7B-H1 (HIGH, VERIFIED) — "no other call-site edits needed" is FALSE.**
  `ResolveProfileArn` (`kiro_api.go:321-330`) has an auth-refresh FALLBACK: if
  cross-region probing fails, it calls `auth.RefreshToken` and caches whatever
  ARN comes back with NO region check. So an override to a region where the
  account has no profile does NOT fail closed — it can cache an out-of-region ARN
  and route it to the override host → host/ARN mismatch, the exact bug the
  override was meant to prevent. My §2b/§2c claim of "no other call-site edits"
  is wrong.
- **7B-H2 (HIGH, VERIFIED) — refresh callers silently undo the pin.** Multiple
  refresh paths (`handler.go:336/349/351`, `handler.go:4841/4852/4854`) accept any
  nonempty `profileArn` from auth and overwrite the cached ARN with no region
  check. A background refresh can repoison the ARN after a manual switch.
- **7B-H3 (HIGH, VERIFIED) — lost-update race.** `apiUpdateAccount` does
  read-modify-write on a full struct copy then `config.UpdateAccount` REPLACES the
  whole struct (`config.go:885-891`). `RefreshAccountInfo` (`kiro_api.go:~768`)
  independently writes a full-struct copy from a pre-change snapshot. A refresh
  in flight during the override change can clobber `RegionOverride` and/or restore
  the old `ProfileArn`. Whole-struct replace is unsafe for this op.
- **7B-H4 (HIGH, VERIFIED) — generation soft-fails open.** `kiro.go:378-386`:
  when `ResolveProfileArn` fails (non-soft error) it only LOGS and dispatches with
  an empty `ProfileArn` (line 397 still runs). So blanking the ARN does not
  guarantee re-resolution-or-fail; a request can go out with no ARN against the
  override host. My §2d "next request re-resolves or fails" claim is wrong.

Plus MEDIUM/LOW items I accept: discovery doesn't verify the discovered ARN's
region matches the pinned region (`kiro_api.go:474-486`); `reresolveProfileArn`
mutates `account.Region` (`kiro_api.go:583-584`); optional model refresh leaves
stale capabilities on failure (needs `ClearModelList`); loose map decoding accepts
non-string `regionOverride`; response-cache key may serve old-region content;
overage paths (`kiro_overage.go:56/135`) ride `regionalizeURL` too and need test
coverage; export/import (`handler.go:~5310`) omits the field; `apiAddAccount` and
credential imports make the field externally writable without validation.

### 7C. CONSOLIDATED VERDICT

**CONSENSUS: CHANGES REQUIRED (major).** The *routing* core (override-first in
`kiroRegionForProfile`) is correct and confirmed, but the plan's claim that this
alone suffices is FALSE. Profile-ARN can enter the account from at least three
paths that bypass the override (auth-refresh fallback inside ResolveProfileArn,
the external refresh callers, and self-heal), and both the ARN-blanking and the
config write have correctness holes (soft-fail-open dispatch, whole-struct race).
The override must be enforced at the ARN layer, not just the URL layer. See §8
for the hardened design that closes all of these.

---

## 8. HARDENED DESIGN (implement THIS, not §2)

Principle: an override is a hard, data-plane-only pin. It must be enforced at
BOTH layers — the URL (region) AND the profile-ARN — and every ARN acquisition
path must refuse an out-of-region ARN. Fail closed, never open.

### 8a. Config field + helpers (`config/config.go`)
- Add `RegionOverride string `json:"regionOverride,omitempty"`` to `Account`.
- `func (a *Account) EffectiveRegionOverride() string` → trimmed, lower-cased.
- NEW atomic patch method (closes 7B-H3 race):
  ```go
  // UpdateAccountRegionOverride sets the override and, when it changed, clears
  // the cached ProfileArn — all under one cfgLock, mutating ONLY these two
  // fields so a concurrent whole-struct RefreshAccountInfo write cannot lose it.
  func UpdateAccountRegionOverride(id, override string) error
  ```
  Do NOT route this through whole-struct `UpdateAccount`.
- Export/import (7B): add `regionOverride` to the account export schema and
  import path (`handler.go:~5310`), or document the omission explicitly.

### 8b. ARN-layer enforcement (`proxy/kiro_api.go`) — the core fix
- NEW guard used everywhere an ARN is accepted:
  ```go
  // arnRegionAllowed reports whether an ARN may be cached for this account given
  // its override. No override → always allowed. Override set → the ARN's embedded
  // region must equal the override.
  func arnRegionAllowed(account *config.Account, arn string) bool {
      ov := account.EffectiveRegionOverride()
      if ov == "" { return true }
      return regionFromProfileArn(arn) == ov
  }
  ```
- `kiroRegionForProfile`: override wins FIRST (as §2b). CONFIRMED-correct routing.
- `kiroProfileRegionCandidates`: when override set → return `[]string{override}`
  ONLY (as §2c).
- `resolveProfileArnAcrossRegions` (loop at `kiro_api.go:474-486`): when override
  set, DROP any discovered ARN failing `arnRegionAllowed` before appending
  (closes 7B medium — discovery trusting cross-region ARNs).
- `ResolveProfileArn` auth-refresh fallback (`kiro_api.go:321-330`) (closes
  7B-H1): before caching `refreshedArn`, check `arnRegionAllowed`; if it fails,
  DO NOT cache — return a hard `"no profile in override region <r>"` error.
- `reresolveProfileArn` (`kiro_api.go:566-589`) (closes 7B medium): REMOVE the
  `account.Region = regionFromProfileArn(newArn)` mutation; enforce
  `arnRegionAllowed` on the new ARN. Add a regression test proving auth refresh
  still receives the ORIGINAL auth region.

### 8c. Refresh callers must not repoison the pin (`proxy/handler.go`) (7B-H2)
Every site that writes a refreshed `profileArn` back to the account
(`handler.go:336/349/351`, `4841/4852/4854`, plus the re-enable/validate flows)
must route through a single helper `acceptRefreshedProfileArn(account, arn)` that
applies `arnRegionAllowed` and skips the write on mismatch. Centralize; do not
duplicate the check.

### 8d. Generation must fail closed (`proxy/kiro.go:378-386`) (7B-H4)
When `account.EffectiveRegionOverride() != ""` and profile resolution yields no
ARN (or a mismatched one), make it a HARD error before dispatch — do NOT fall
through to `regionalizeURLForProfile` with an empty ARN. Without an override,
preserve today's soft-fail behavior exactly (backward-compatible).

### 8e. Model cache + cooldown hygiene on change (`pool`, `proxy`)
- Add `pool.ClearModelList(accountID)` and call it when the override changes, so
  a stale old-region model set can't keep the account marked model-capable
  (`pool/account.go:230-234`) (closes 7B medium).
- On override change, delete the profile-ARN resolution cooldown key
  (`profileArnResolutionCooldowns`, `kiro_api.go:355/374`) so the switch always
  re-probes (my 7A-F2, still valid).
- Model refresh after change must be validate-then-commit, not fire-and-forget.

### 8f. API validation (`proxy/handler.go`) (7B medium)
- Use a TYPED patch for `regionOverride` (`*string`), reject wrong JSON types
  with HTTP 400 (not silent ignore).
- Validate loosely but safely for host construction: lower-case DNS-label shape,
  bounded length, `us-gov-*`/multi-part allowed (fix my 7A-F1 brittle regex).
  Empty always clears.
- Apply the SAME validation/normalization in `apiAddAccount` (`handler.go:3065`)
  and credential-import construction (`handler.go:~3912`, `cli_json_import.go:160`)
  — the field is externally writable there too.
- Same-value-but-mismatched-ARN case: if the requested override equals the
  current one BUT the cached ARN's region differs, still clear/re-resolve the ARN
  (don't gate re-resolution on override-value change alone).

### 8g. Response cache (7B low)
Audit the exact-match response-cache key; include effective data-plane region (or
invalidate on override change) so post-switch requests can't serve old-region
cached content.

### 8h. Tests (mandatory, handler-level included — 7B)
Beyond §5: `arnRegionAllowed` table; auth-refresh-fallback rejects wrong-region
ARN; refresh caller skips repoisoning; generation hard-fails with override+no
ARN; discovery drops cross-region ARNs; `reresolveProfileArn` preserves
`account.Region`; atomic `UpdateAccountRegionOverride` survives a concurrent
whole-struct write; typed 400 on bad JSON; `apiAddAccount`/import validation;
overage GET/SET under override; export/import round-trip; model-cache cleared on
change. Handler tests are REQUIRED (harness exists: `handler_test.go:48/56`).

### 8i. Verification gate
As §6, plus: a concurrency test (override change vs. in-flight `RefreshAccountInfo`)
and a live check that an override to a profile-less region FAILS CLOSED (no
request dispatched with an empty/cross-region ARN) rather than silently routing.
