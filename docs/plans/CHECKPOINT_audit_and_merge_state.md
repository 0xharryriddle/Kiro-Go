# Checkpoint — audit state, merge state, remaining work

Purpose: a resumable record of where this project stands, written so a fresh
session (or a reviewer) can pick it up without re-deriving anything. Every
claim below is verified against the live tree by real command output; items I
could not verify are labelled as such rather than asserted.

Last verified: repo `harry` branch, **merge COMMITTED** at `e902ed3`, 49 ahead of
`origin/harry` — see §1 (SECOND merge, `hian699` v1.2.8). The §1 table below was
rewritten on that date; the v1.1.5 numbers it used to carry are preserved in §1a so
the older record is not lost.

---

## 1. Verified state of the tree

**This is a SECOND, DIFFERENT merge from the one the rest of this document
describes.** §2-§3 and R1 concern upstream `v1.1.5` (`ec4ba56`), which is long since
committed and pushed. This second merge is now committed too, but **not pushed**.

| Fact | Value | How verified |
|---|---|---|
| Branch | `harry` | `git rev-parse --abbrev-ref HEAD` |
| HEAD | `e902ed3` (`merge: resolve hian699 v1.2.8 into harry (48 commits)`) | `git rev-parse HEAD` |
| Merge in progress | **NO** — `MERGE_HEAD` gone, merge concluded | `git status` |
| Merge parents | `9f0b943` (ours) + `8a2dfc4` (theirs) | `git log -1 --format=%p` |
| Incoming remote | `hian699` → `https://github.com/hian699/Kiro-Go` | `git remote -v` |
| Merge base | `a2e3971` | `git merge-base 9f0b943 8a2dfc4` |
| Incoming commits | **48** | `git rev-list --count 9f0b943..8a2dfc4` |
| Version in tree | `1.2.8` (`config/config.go:783`), `version.json` agrees | `grep`, `cat version.json` |
| Unmerged paths | **0** | `git diff --name-only --diff-filter=U` (empty) |
| Conflict markers remaining | **0** across all tracked files | `git grep -nE '^(<{7}\|={7}\|>{7})'` → 0 hits |
| Committed | **YES** — `e902ed3` | `git log -1` |
| Pushed to `origin/harry` | **NO — 49 ahead / 0 behind.** `origin/harry` still at `9f0b943`. Push needs user authorization. | `git rev-list --left-right --count HEAD...origin/harry` → `49  0` |
| Build | clean | `go build ./...` |
| Vet | clean | `go vet ./...` |
| `gofmt -l` | clean (empty) | `gofmt -l .` |
| Test suite | **1328 passed, 0 failed** across 6 packages | `go test ./... -count=1` |
| `-race` | clean, 0 data races | `go test ./... -race -count=1` |
| Top-level test funcs | **1063** — config 77, pool 91, auth 63, proxy 832 | `grep -rhoE '^func Test[A-Za-z0-9_]+\(' <pkg>/*_test.go \| sort -u \| wc -l` |
| Source / test lines | 47,005 src / 35,786 test | `find … -exec cat {} + \| wc -l` |
| Live container | **NOT RUNNING** — `docker ps` returns no `kiro-go` row | `docker ps --filter name=kiro-go` |

**A correction to my own reading, recorded rather than silently fixed.** I first
reported `MERGE_HEAD` as `b3fa616` ("Add DoS guard, per-key limits, Force Model…").
That was wrong: the terminal wrapper dropped a line from a multi-line `git log`
block, so two separate outputs read as one. `MERGE_HEAD` is `8a2dfc4`, and
`b3fa616` is merely the newest commit in the incoming range. Re-measured by writing
each value to a file with an explicit `key=value` label and reading it back — the
right technique whenever this terminal mangles multi-line output.

**Interpretation.** All conflicts are resolved in the working tree and the full gate
is green from a cold build cache, but nothing is committed: `MERGE_HEAD` is still
set, so this is a merge awaiting its commit, not a finished one.

### 1a. Superseded — state at the v1.1.5 merge (historical)

Kept so the older record survives the §1 rewrite: HEAD was `6158c24`, a real
two-parent merge of `99dda52` + `ec4ba56`, `MERGE_HEAD` cleared, 0 conflict markers,
**826 tests** passing, `-race`/vet/gofmt clean. It arrived *resolved but unstaged*
(git reported `U` for 14 paths with no markers in any of them — an earlier session
resolved them in place without `git add`); every path was re-verified marker-free and
the gate re-run from the exact staged state before committing. That merge was later
pushed; see R1.

---

## 2. What upstream v1.1.5 brought in (9 commits)

```
ec4ba56  chore: bump version to 1.1.5
40f5e25  fix: harden web search handling and add regression tests
9409c3f  feat: Anthropic native web_search via Kiro MCP (#120)
95c85db  fix: stop dropping repeated content from response streams (#138)
df13a66  fix: classify major-only Claude versions (opus-5) as 1M context (#140)
f0e2e61  fix: app.js syntax error + stabilize API key import tests
1e67432  feat: Kiro API key credentials and import
b0ebb56  fix: Microsoft SSO import trust boundaries + credential copy fields
82247ca  feat: Microsoft Enterprise SSO
```

Two notes that matter:

- **`df13a66` is the same Opus 5 1M-context fix I made independently.** Upstream
  reached the same conclusion. The merged tree keeps a working version; the
  regex at `proxy/kiro.go` still classifies bare-major `claude-opus-5` as 1M.
- **`9409c3f` adds native `web_search` through Kiro MCP** — a new tool surface
  that my audit never examined. See §4 item R4.

---

## 3. Audit checkpoint — 18 defects fixed, all verified surviving the merge

Committed as `d81cb7b` ("fix(audit): 18 verified defects across auth, pool,
config, translation, streaming"), which **is** an ancestor of HEAD.

Each was proven with a RED test against real source before the fix, then
verified GREEN. Survival re-checked against the post-merge worktree:

| # | Defect | Site | Post-merge |
|---|---|---|---|
| 1 | Bare-major flagship models got 200K instead of 1M context | `proxy/kiro.go` | PRESENT (also fixed upstream) |
| 2 | Dated snapshots misparsed as 1M (unbounded minor group) | `proxy/kiro.go` | PRESENT |
| 3 | `thinking.budget_tokens` validated then discarded | `proxy/translator.go` | PRESENT |
| 4 | `cache_control` dropped at decode on `ClaudeTool` | `proxy/translator.go` | PRESENT |
| 5 | `cache_control` dropped at decode on `ClaudeContentBlock` | `proxy/translator.go` | PRESENT |
| 6 | Parallel tool-use fragments merged / second call dropped | `proxy/kiro.go` | PRESENT |
| 7 | `tool_result.is_error` discarded — failed tools reported success | `proxy/translator.go` | PRESENT |
| 8 | Data race: routing getters returned `&p.accounts[i]` | `pool/account.go` | PRESENT (copy-return) |
| 9 | Data race: `Init` wrote `cfgPath` unlocked vs `Save` | `config/config.go` | PRESENT |
| 10 | Stats snapshots could move counters backwards | `config/config.go` | PRESENT (monotonic) |
| 11 | Diagnostics reported quota-exhausted accounts as Available | `pool/account.go` | PRESENT (reordered) |
| 12 | 4 byte-truncations split UTF-8 runes | `proxy/translator.go` | PRESENT |
| 13 | Unbounded alloc from 4-byte frame length (192MiB from 12B) | `proxy/kiro.go` | PRESENT — renamed by merge to `maxEventStreamMessageBytes` + `errEventStreamFrameTooLarge` |
| 14 | Claude stream: unclosed blocks, no `message_stop` mid-stream | `proxy/handler.go` | PRESENT |
| 15 | OpenAI stream: no error, no `finish_reason`, no `[DONE]` | `proxy/handler.go` | PRESENT |
| 16 | Responses stream: no `[DONE]`, failure not attributed to account | `proxy/responses_handler.go` | PRESENT |
| 17 | Healthy accounts permanently BANNED by body-substring match | `proxy/account_failover.go` | PRESENT (status-anchored, 3 formats) |
| 18 | Live `refresh_token` written into a logged error string | `auth/oidc.go` | PRESENT |

**Correction to an earlier report:** I initially flagged 6 of these as MISSING
post-merge. That was wrong — my grep patterns were fragile (brace/paren escaping
under a shadowed `rg`). Re-verified individually: all 18 survive. #13 survives
under a different, better name chosen by upstream.

### Findings I investigated and rejected (do not re-fix)

- Cache fingerprints including `cache_control` marker position —
  `writeCanonicalJSON` already strips the key at every map level. Code was correct.
- Auth session-timer race (`auth/kiro_sso.go`) — subagent claim; produced **zero**
  races under `-race` across ~2000 iterations. Unsubstantiated.
- OpenAI-route `is_error` equivalent — the OpenAI wire format has no such field
  on a tool message. The asymmetry with the Claude route is correct.
- Responses route has no prompt-cache/thinking-budget plumbing — correct, that
  wire format carries neither field.

---

## 3b. Round-2 audit — 18 further defects in the new upstream surfaces

Method: three independent reviewers (`gpt-5.6-sol-thinking`, reasoning_effort
max) audited websearch/MCP, Microsoft SSO, and pool routing from source. Every
claim was then re-derived by the parent against live bytes — reviewer
self-reports were treated as leads, not facts. Each fix below was RED-proven
first (test fails against the unfixed code) and several were additionally
verified by neutralizing the fix and confirming the test goes red again.

| # | Defect | Site | Why it mattered |
|---|---|---|---|
| 19 | F3 external-usage auto-disable was **unreachable dead code** — gate required `strong_external && acc.Enabled`, but that tier is only assigned when `!EnabledLocally` | `proxy/handler.go:623` | Feature marked SHIPPED had never once fired; zero test coverage hid it |
| 20 | Auto-recovery re-enabled accounts quarantined for external usage within one 60s tick | `pool/account.go` `reprobeDisabled` | A successful token refresh is not evidence a shared credential stopped being shared; F3 undid itself |
| 21 | HTTP status classified *after* JSON parse, so a genuine 401 answered with an HTML error page became an unclassifiable parse error | `auth/microsoft_sso.go` `postExternalIdpToken` | `isAuthErrorMessage` had no status to anchor on → real credential failures missed |
| 22 | Submitted credentials echoed into the returned error via `error_description` | same | Error is logged verbatim by the background refresher → live token in operator logs |
| 23 | `isAuthErrorMessage` let body markers vote on **5xx** | `proxy/account_failover.go:191` | An upstream outage whose body merely mentioned `invalid_grant` permanently BANNED a healthy account — the exact bug class the comment above it claimed was fixed |
| 24 | `pool.IsAuthFailure` had **no status gating at all** (sibling of #23) | `pool/account.go` | Same permanent ban, reached via `classifyAndBanOnUsageError` |
| 25 | Unbounded `io.ReadAll` on the MCP response body | `proxy/websearch.go` | Body is amplified twice downstream (result blocks + summary); sibling call sites already bound with `io.LimitReader` |
| 26 | Quota-aware routing (F2) **bypassed the circuit breaker** | `pool/account.go` `eligibleForRoute` | The account with most remaining quota is exactly the one a fresh ban leaves untouched — enabling F2 silently disabled the breaker |
| 27 | Quota-aware dispatch never stamped the LRU clock | `pool/account.go` | Flipping the toggle off handed that account a burst of consecutive requests |
| 28 | `Reload` never pruned per-account state; a deleted+re-added ID **inherited the old OPEN breaker**, cooldown and error count | `pool/account.go` `Reload` | Operator re-adding a credential to fix a problem got an account that looked broken for no visible reason |
| 29 | `maxAffinityEntries` was decorative — expiry-only pruning is not a bound | `pool/account.go` | One request per random API key grew the map without limit |
| 30 | Half-open breaker admitted **unlimited** concurrent probes | `pool/account.go` `isOpen` | Full request rate resumed the instant the window elapsed, against an account that had just failed 5× |
| 31 | `DisableAccount`/`MarkOverLimit` set their safety-net cooldown *before* `Reload`, which prunes it | `pool/account.go` | A value whose stated job is to survive a racing Reload was deleted by it |
| 32 | Cooldown fallback dispatched to accounts with an **open circuit** | `pool/account.go` `fallbackEarliestCooldown` | Converted one open breaker into a stream of failed requests, each re-arming it |
| 33 | Session-affinity TOCTOU returned an account that a completed `Reload` had removed | `pool/account.go` `GetNextForModelWithApiKey` | Dispatched on a credential already pulled from routing |
| 34 | Redaction could not catch an IdP-**returned** rotated token, nor a submitted value under the 16-byte floor | `auth/microsoft_sso.go` | Name-based (`param=value`) redaction added; prose that only *mentions* a parameter still survives |
| 35 | `max_uses` exhaustion rendered as a **successful empty result** | `proxy/websearch_loop.go` | Indistinguishable from "search ran, found nothing"; contract requires `web_search_tool_result_error` / `max_uses_exceeded` |
| 36 | MCP JSON-RPC `id`/`jsonrpc` never validated against the request | `proxy/websearch.go` | A replayed/reordered reply could return one query's results as the answer to another |

Also removed: an unrequested stream-delta probe a reviewer had wired into the
streaming hot path (`proxy/kiro.go`) whose counters no route ever read.

### Round-2 claims investigated and REJECTED (do not re-fix)

- "Untrusted search text reaches the model summary" — inherent to any web-search
  tool; the content *is* the payload. Not a defect.
- "Result expansion has no aggregate bound" — real but subsumed by #25, which
  bounds the input that feeds the expansion.
- Reviewer #3's probes for half-open single-probe, reused-ID inheritance,
  quota-aware bypass and affinity bound asserted via unconditional `t.Fatalf`,
  so they "confirmed" regardless of behaviour. Re-tested with correct
  assertions: 4 of 9 were real (#26–#29), the rest were artifacts.
- Concurrent model-map / affinity / dispatch-seq access — clean under `-race`.

---

## 3c. Round-3 — self-audit of the round-2 fixes

My own round-2 changes had received no independent scrutiny, so before landing
them I audited them myself (and dispatched a fresh adversarial review of the
diff). Four defects found in MY OWN fixes plus one pre-existing latent panic, all
RED-proven and fixed:

| # | Defect | Site | Notes |
|---|---|---|---|
| 37 | **Regression I introduced.** `isOpen` both decided AND mutated (promoted open→half-open, stamped `probeAt`). Adding two call sites meant ONE selection pass consumed its own probe: gate A promoted and admitted, gate B saw a probe in flight and blocked — taking a single-account pool dark exactly when its breaker was due a recovery probe | `pool/account.go` | Fixed by splitting the pure predicate `isOpen` from an explicit `claimProbe`, called once at each of the 4 dispatch commit points. RED-proven by neutralizing `claimProbe` → 4 tests fail |
| 38 | **Pre-existing latent panic** (not mine, surfaced while auditing my affinity work): the affinity bind writes `p.apiKeyAffinity` unconditionally, but that map is only populated by `GetPool()`. A pool assembled field-by-field reaches it nil → *write to nil map panics* → whole proxy process dies instead of degrading to no-affinity | `pool/account.go` | Guarded, matching how `lastDispatchSeq` is already handled two lines below |
| 39 | **Regression I introduced.** Removing the `acc.Enabled` guard from F3 (fix #19) let it stamp an account that was already `BANNED`. `SetAccountBanStatus` overwrites unconditionally, so a permanent operator-only ban was DOWNGRADED to `DISABLED` — which auto-recovery is allowed to revisit. External usage could un-ban a credential banned for a more serious reason | `proxy/handler.go` | Fixed with an `alreadyBanned` guard; positive control proves a merely-disabled account is still quarantined |
| 40 | **Over-reach I introduced, now walked back.** Fix #36 REJECTED an MCP response whose JSON-RPC id/version mismatched. But nothing in this repo records a real MCP response, so "Kiro echoes our id back" was an unverified assumption — if the server picks its own id, failing closed breaks EVERY web search and burns one account per attempt. The security value is also small: one-shot HTTP already binds reply to request via the connection | `proxy/websearch.go` | Downgraded to a loud warning. Detection kept (a real mismatch is discoverable from logs), rejection deferred until a capture proves the id is mirrored |
| 41 | Client-facing status consequence of fix #23 was unpinned: a 5xx carrying an auth marker now correctly returns 502 rather than 401 | `proxy/account_failover.go` | Pinned in both directions |

Also verified rather than assumed:
- **Lock discipline** across all new `*Locked` helpers — every caller holds the
  write lock; `Reload` still reads config BEFORE taking `p.mu`, preserving the
  documented `p.mu → cfgLock` leaf ordering. No lock-order inversion introduced.
- **Redactor edge cases** — 13 cases including the overlapping-name traps
  (`client_assertion` contains `assertion`; `device_code` contains `code`),
  underscore-prefixed lookalikes (`error_code=` must NOT match `code`),
  case-insensitivity, and multibyte preservation (byte-index alignment is why
  `asciiLower` exists rather than `strings.ToLower`).
- **Affinity eviction cost** — MEASURED, not hand-waved: ~20.6µs per eviction at
  the 1024 cap under the write lock (`BenchmarkEvictOldestAffinityAtCap`).
  Negligible against the Kiro round-trip, and only reachable under API-key
  rotation abuse. Recorded in the code comment with the escape hatch (heap /
  intrusive LRU) if that changes.
- **Multi-skip `max_uses` bookkeeping** — every remaining `web_search` in a round
  is marked, client tools are not.

Lesson worth keeping: a query that mutates as a side effect is safe with one
caller and unsafe with two. Fix #26/#32 added callers to `isOpen` without
noticing it was not a pure predicate. That is exactly the class of bug an
independent reviewer of the DIFF catches and a reviewer of the ORIGINAL code
cannot.

### Round-3b — defects found by reviewing the round-2/3 fixes AGAIN

A second adversarial pass over the same diff (reviewers probing the redactor and
the classifiers with real Azure AD / formatter strings) surfaced four more, three
of them regressions introduced by MY earlier fixes:

| # | Defect | Site | Notes |
|---|---|---|---|
| 42 | **Regression I introduced (functional, not cosmetic).** The name-based redactor accepted a BARE colon as an assignment, so real IdP prose was shredded: `"the code: invalid_grant was already redeemed"` → `code: [REDACTED]`. That destroys the `invalid_grant` marker `isAuthErrorMessage` classifies revoked credentials by — a genuinely revoked token became unclassifiable and the account was never flagged for re-auth. Also ate `"error code: 50173"`, `"status code: 401"`, and URLs after a colon | `auth/microsoft_sso.go` | Narrowed to machine syntax only: `=` always, `:` only when the key is JSON-quoted (`"name": "value"`). 6 prose cases + 2 machine-assignment controls pin both directions |
| 43 | Boundary check treated `-` as a name byte, so `x-refresh_token=<secret>` was skipped as an unrelated identifier and leaked | same | `-` no longer binds a name; `_`/alphanumerics still do (so `error_code=` is still not `code`) |
| 44 | **Regression I introduced.** `upstreamStatusFromMessage` selected by PATTERN ORDER, not string position. For `refresh failed: 401 {..."trace":"HTTP 503 from edge"}` it returned **503**, so fix #23's 5xx gate then refused to ban a genuinely revoked credential. The body outvoted the header | `proxy/account_failover.go` | Now returns the LEFTMOST match across all patterns — every formatter writes the authoritative status at the front |
| 45 | Same positional flaw in the pool sibling, reached differently: `hasUpstream5xxStatusToken` matched any bare 5xx token ANYWHERE, so `refresh failed: 400 {..."upstream returned 500"}` hit the 5xx gate and a revoked credential was read as a server outage | `pool/account.go` | Replaced with `firstUpstreamStatusToken` (leftmost 400–599, same boundary rule). The two classifiers now agree on all 18 real formatter strings — pinned by a permanent cross-package agreement test so they cannot silently diverge again |

Verified rather than assumed in this pass:
- **Redactor is panic-free and terminating** under a brute-force probe over
  `name` + up to 3 arbitrary bytes (including invalid UTF-8), and never turns
  valid UTF-8 input into invalid output.
- **Accepted redactor limits**, documented in code so nobody "fixes" them into
  over-redaction: `refresh_token_value=` (different parameter), and spelling
  variants we never submit (`refreshToken=`, `refresh-token=`, bare `token=`).
  The value-based pass still covers those whenever the value is one we sent.

Reviewer artifacts: all `zz_*` probe files were transient and are gone (count
verified 0). Their findings were re-derived against live bytes before any fix;
one reported DIVERGE line turned out to be stale `/tmp` output from a probe its
author had already deleted, which is why probe claims get re-run rather than
believed.

### Round-3c — a third pass over the same diff

Reviewers kept probing the fixes from rounds 2/3. Three more defects, all in MY
own changes:

| # | Defect | Site | Notes |
|---|---|---|---|
| 46 | **Regression I introduced, and the fix bought nothing.** Fix #31 reordered `DisableAccount`/`MarkOverLimit` to Reload BEFORE stamping the cooldown. Proven by probe: that opens a window where the account is back in `p.accounts` with no cooldown — fully routable, which is precisely what these calls exist to prevent. Worse, the justification was wrong: the old order's cooldown already survived the prune, because the prune keys off `config.GetAccounts()` (all configured accounts), not the enabled subset | `pool/account.go` | Now stamps on BOTH sides of Reload via `setCooldownIfLater` (idempotent): before, so no window exists; after, so a config-absent id survives the prune. Neither order works alone |
| 47 | `MarkOverLimit` used a raw map assignment, so a 402 arriving while a longer backoff was in force (1h quota, or the 24h disable safety net) SHORTENED it and pulled the account back into rotation early | `pool/account.go` | Switched to `setCooldownIfLater`, matching `RecordError`'s existing discipline |
| 48 | **Regression I introduced.** Fix #45's `firstUpstreamStatusToken` matched BARE numbers in 400–599, so `unauthorized (usage 512/1000 credits)`, `token expired 540 seconds ago`, `[seq 501]` and `remaining balance 550` all read as server statuses and suppressed a genuine credential failure. Inverted the classifier: a revoked credential filed as an outage, never flagged for re-auth, kept being routed while every request failed | `pool/account.go` | A number now only counts as a status when INTRODUCED as one (`http`/`status`/`returned`/`failed` before it), mirroring proxy's patterns. Critically, the context requirement applies ONLY to the 5xx suppression — a bare 401/403 stays positive evidence, because that branch can only ever CLEAR a failure |

Note on #48: the first attempt at it broke `"received 403 Forbidden"` (two
pre-existing tests caught it). That is why the context rule is asymmetric —
requiring context to DETECT auth is unsafe, requiring it to DISMISS auth is safe.

Cumulative: **48 defects** fixed across three rounds. Nine were regressions
introduced by earlier fixes in this same series, every one caught by re-reviewing
the diff rather than the original code.

### Round-3d — findings from the second reviewer batch (opus-5)

A second batch of reviewers (this one pinned to the parent model rather than
gpt-5.6-sol) re-reviewed the same diff. Its two HIGH findings — status chosen by
pattern order, and bare numbers read as 5xx statuses — were **already fixed** as
#44/#45/#48 before the batch reported; they were independently re-derived, which
corroborates those fixes rather than adding work. Two genuinely new defects:

| # | Defect | Site | Notes |
|---|---|---|---|
| 49 | Name-based redaction matched only exact snake_case, so the one spelling this codebase's own upstream actually emits was uncovered: `auth/oidc.go:291-292` and `config/config.go:65-66` declare these fields as `json:"refreshToken"` / `json:"accessToken"`. A gateway echoing a ROTATED credential in that shape leaked it — and a rotated value is one we never submitted, so the value-based pass cannot catch it either. Name matching was the only control on that case and it did not know the name | `auth/microsoft_sso.go` | Each sensitive parameter now expands to snake_case, hyphenated, and separator-stripped (camelCase-matching) spellings, longest-first so a longer parameter is never half-matched by a shorter one |
| 50 | The two redaction passes corrupted each other's output: `]` was a value terminator but `[` was not, so the assignment pass stopped just before the closing bracket of a `[REDACTED]` the value pass had written and re-emitted the orphan — yielding `[REDACTED]]`, growing on every re-application | `auth/microsoft_sso.go` | `[` is now a terminator too. No secret ever escaped, but a redactor that corrupts its own output invites doubt about what else it rewrote; idempotence is now pinned by test |

Cumulative: **50 defects**.

Also noted by that batch and NOT actioned, with reasons:
- `fallbackEarliestCooldown` skipping open breakers (#32) trades "always serve
  something" for "serve nothing for up to `circuitOpenDuration`" on a
  single-account pool. That is the intended trade — dispatching into an open
  breaker converts one open breaker into a stream of failures — and recovery is
  bounded and proven by `TestOpenCircuitRecoversViaProbeAfterWindow`. Flagged as
  a product decision, not a defect.
- Assorted redactor gaps that require no delimiter at all (`refresh_token is now
  <value>`, NBSP/thin-space before `=`, `refresh_token -> <value>`). Closing
  these means treating prose as assignment, which is exactly the over-redaction
  that broke auth classification in #42. Documented as accepted limits in code.

One process note worth keeping: reviewer #1 reported that the file changed under
it mid-review (line count moved five times, and it observed `claimProbe`
temporarily stubbed to `_ = now` — my own RED-proof neutralization). Its
conclusions were still sound because it pinned snapshots, but reviewing a live
working tree is unreliable by construction. Future adversarial passes should
review a committed revision.

The third reviewer in that batch (websearch/handler/kiro) **failed with no
output**, so those surfaces were left unreviewed and a replacement was dispatched
against the committed state.

### Round-4 — live-deployment audit (the merge was never actually running)

Everything above was verified in the TREE. This round checked the running
container and found the tree and the deployment had diverged, then found two
defects that only a live probe could have surfaced.

| # | Defect | Site | Notes |
|---|---|---|---|
| 51 | **The container was running pre-merge code for ~37 hours.** Image built 2026-07-25 15:37, reporting version 1.1.2; the merge and every fix postdated it. `docker compose build` compiles from source at build time, so committing and pushing changed nothing about what served traffic. Verified by symbol check on the in-container binary: every new symbol ABSENT, with a positive control confirming the probe worked | deployment | Rebuilt and swapped; live now reports 1.1.5 with all symbols present |
| 52 | **`ListAvailableProfiles` sent `maxResults: 50`, which the live endpoint REJECTS** with HTTP 400 `REQUEST_BODY_INVALID`. Boundary swept against the real service: 1/5/10 pass, 11/15/20/25/30/40/49/50/100 all fail — the limit is exactly 10. The pre-merge fork sent 10; upstream v1.1.5's paginated rewrite hardcoded 50 and the merge adopted it. Consequence chain: 400 → no `profileArn` → `GetUsageLimits` fails → subscription/usage never populate → the admin UI displays **"Free" on a genuine paid plan** | `proxy/kiro_api.go` | Now `kiroProfilePageSize = 10` with the sweep recorded in the comment. Pagination still honours `nextToken`, so the smaller page costs at most one extra round-trip per 10 profiles |
| 53 | **An upstream test was actively hiding #52.** `TestListKiroProfilesFollowsNextTokenPagination` asserted `maxResults:50` against a MOCK transport that accepts any body. A mock more permissive than the real service is exactly how a 400-on-every-call bug reaches production | `proxy/kiro_api_test.go` | Assertion now pins the constant, not a literal, so the two cannot drift |
| 54 | **The unauthenticated `/v1/models` route could drive the whole fleet into cooldown.** No auth, refreshes whenever the aggregate cache is empty, and `refreshModelsCache` deliberately installs an EMPTY aggregate when every account fails — so while the fleet was unhealthy the empty-cache condition stayed true and every anonymous request repeated a full sweep: `ensureValidToken` + `ListAvailableModels` + `handleAccountFailure` for every enabled account, at HTTP request rate. `modelsCacheTime` existed but was **write-only** — never compared anywhere, so there was no TTL guard at all | `proxy/handler.go` | Throttled to one attempt per 60s, keyed on ATTEMPTS not successes (a success-only stamp never arms while failing, which is the whole failure mode). Explicit invalidation clears the stamp so profile switches still rebuild immediately. RED-proven: neutralized → 10 anonymous requests = 10 full fleet sweeps |

Method note that produced #52: hand-rolled HTTP probes gave a plausible but WRONG
answer (one returned "profiles=1", which I reported before re-running and getting
`UnknownOperationException` — the probe was malformed). Driving Kiro-Go's own Go
code paths instead is authoritative by construction, and that is what produced
the real diagnosis. Retracted the earlier claim rather than building on it.

Cumulative: **54 defects**.

### Unauthenticated surface — fully mapped

| Route | Auth | Can it do expensive/account-affecting work? | Status |
|---|---|---|---|
| `/v1/models` | none | YES — was a full fleet sweep per request | FIXED (#54, throttled) |
| `/v1/stats` | none | No — pure in-memory reads, no upstream calls, no mutation | Disclosure only; awaiting user decision |
| `/metrics` | none | No — and config-gated, 404s unless `MetricsEnabled` | Documented, default OFF |
| everything else | API key or admin password | — | — |

`refreshModelsCache`'s other three callers were checked for a bypass: two are the
30-minute background ticker, one is an admin-authenticated endpoint. `/v1/models`
was the only unauthenticated path into it.

### Subagent reliability note

A three-reviewer batch **died silently**: the dispatch returned
`database is locked`, and although the delegation directories were created (which
I checked at the time and misread as "they are running"), the workers never
executed. Their logs sat frozen at 1807 bytes — kickoff header only — for 29
minutes. Re-dispatched successfully against the current HEAD.

Rule: a created delegation directory is NOT evidence of a live worker. Log GROWTH
is. Check `stat -c %s` twice, a minute apart.

### Round-5 — the client-visible error contract

Everything above concerned whether the proxy *works*. This round concerned what
the proxy *tells the client when it fails*, which decides whether the client's
retry policy can be correct. Landed as `3242fff`, `5172d4b`, `5b6fa90`.

Not counted as a defect: `7bfbbb0` introduced `claudeErrorTypeForStatus` because
the mid-stream Claude `error` SSE event hardcoded `api_error` for every failure
while the OpenAI stream already classified the same error. That is a `feat` — a
missing capability, not a broken one — which is why the cumulative count below is
62 and not 63.

| # | Defect | Site | Notes |
|---|---|---|---|
| 55 | **402 reported as `invalid_request_error`, must be `billing_error`.** Verified against docs.anthropic.com/en/api/errors, fetched live (HTTP 200). On this proxy 402 IS the overage case, so this was the likeliest of the three to fire — and it routed a payment condition into the client's malformed-request branch, so "check your billing" was never surfaced | `proxy/account_failover.go` | Mapping written from recollection of the enum; 3 of 10 entries were wrong |
| 56 | **503 reported as `overloaded_error`, must be `api_error`.** The doc pairs `overloaded_error` with 529 only and never lists 503. Telling a client "retry, the API is busy" when the real condition is an unexpected server-side failure invites a retry storm against a fault that will not clear | `proxy/account_failover.go` | 529 now mapped too, though `statusForUpstreamError` cannot yet emit it — an omitted documented pairing is latent the moment a caller passes one through |
| 57 | **504 unmapped, fell through to `api_error`; now `timeout_error`.** The type carries an actionable signal (retry, or stream long requests) that `api_error` does not | `proxy/account_failover.go` | Allow-list test now sweeps statuses 0–599 and asserts the result is always a real Anthropic type, so no client `switch` on `error.type` can hit an unknown branch |
| 58 | **`:message-type: error` frames were silently discarded.** The sibling Bedrock reader treats BOTH `exception` and `error` as failures, on either `:message-type` or `:event-type`, plus any non-empty `:exception-type`. The Kiro branch matched only `exception`, so the identical upstream condition was observable on Bedrock and invisible on Kiro — defeating the purpose of adding observation at all | `proxy/kiro.go` | Factored into `isUpstreamFailureFrame` / `upstreamFailureLabel`, shared predicate and label precedence with the Bedrock reader |
| 59 | **Token accounting was dropped for failure frames.** The `continue` skipped `updateTokensFromEvent`, which every frame reached before that branch existed. A failure frame can still carry a usage block and the upstream charges for those tokens either way — so the skip silently stopped billing for them | `proxy/kiro.go` | Usage is now counted BEFORE the skip |
| 60 | **The logged failure label was unbounded upstream-controlled text.** The label comes from an event-stream string header whose 16-bit length field permits ~64 KiB, logged verbatim. Proven: a 60,050-byte label containing newlines was accepted, so a hostile or malfunctioning upstream could write 60 KB into the operator log per failed frame AND forge additional log lines | `proxy/kiro.go` | Truncated to 200 bytes and marked when shortened. The observer callback still receives the full label — the bound is on what reaches the log |
| 61 | **`errorTypeForOpenAIStatus` had no 4xx branch beyond 401/429**, so 400, 402 and 413 all fell through to `server_error`. Every one is a CLIENT-side condition and the mislabelling misleads in the damaging direction: a 400 retried unchanged can never succeed, a 402 retried can never clear the cap, a 413 needs the conversation SHRUNK not resent. So the caller burns quota and latency on a request guaranteed to fail again | `proxy/account_failover.go` | Found while auditing the Claude-side fix — the exact mirror of the asymmetry that fix removed. New cross-surface test requires the fault CLASS to agree across both surfaces; it failed on 400 and 402 before the change |

Method note: #58–#60 came from an independent reviewer whose probe assertions were
deliberately INVERTED (they pass while the gap exists), so the proof each fix
works is that its probe now fails with "gap does not exist".

### Round-6 — the last unbounded admin surface

| # | Defect | Site | Notes |
|---|---|---|---|
| 62 | **Seven unbounded `json.NewDecoder(r.Body)` calls.** The one admin surface in the package that did not bound its input, while ten routes elsewhere already wrap `r.Body` in `http.MaxBytesReader` at 16 KiB–1 MiB. Proven against the real handler: `POST /admin/new_api_key` accepted a 4,194,329-byte body and returned HTTP 200 — it minted a real customer API key from an oversized request | `proxy/admin_bot_api.go` | One shared `adminBotBodyLimit` (64 KiB) + `decodeAdminBotBody`, applied to all seven sites; per-route drift is what let this file diverge |

Precision worth keeping: `/admin/delete_api_key` and `/admin/recharge_api_key`
appeared to "reject" the same oversized body, but only because they 400/404 on a
missing key identifier — validation firing AFTER the unbounded read, not a size
bound. All seven sites were equally unbounded; only one succeeded far enough to
prove it.

### Round-7 — a safety invariant with no test (`5c748be`)

No defect. `proxy/token_estimator.go` had no test file at all while carrying a
property nothing pinned: `estimateApproxTokens` (reporting) is deliberately
OPTIMISTIC (4.5 ASCII / 1.5 non-ASCII) and `estimateWireTokens` (bounding a
forwarded request) deliberately PESSIMISTIC (4.0 / 1.0). If the wire estimate ever
UNDER-counts the reported figure, an oversized request goes upstream and the whole
call is rejected. Concrete rather than theoretical, because
`estimateApproxTokensWith` short-circuits strings under 5 runes past the weight
table entirely and divides by a hardcoded 3.0 — that path could invert the
relation with no weight change.

Audited: the invariant HOLDS across 250,000 fuzz cases. RED-proven by loosening
`wireTokenWeights` to match the reporting table — 105,910 short violations (worst
margin −4) and 49,936 long (worst margin −80) are detected, so the test
discriminates.

Cumulative: **62 defects** — this is the figure the handoff prompt cites. The
count above previously stopped at 54 because rounds 5–7 landed as commits without
being written back here; that gap is now closed.

### Round-8 — billing/credential surfaces (this session)

| # | Defect | Site | Notes |
|---|---|---|---|
| 63 | **`GET /api/me` returned `keyMasked: ""` on every request.** It masked `entry.Key`, but keys are hashed at rest — `AddApiKey` clears `Key` and returns the cleartext exactly once at mint time — so the field was ALWAYS empty, silently breaking the one thing it exists for: letting a customer confirm which credential they queried with. The sibling admin view already used the correct helper | `proxy/customer_api.go:124` | Now `config.ApiKeyDisplayMask(*entry)`, which prefers the stored `KeyMask`. RED-proven: the new test asserted non-empty and failed against the unfixed code |
| 64 | **A recharge re-enabled a key an OPERATOR had disabled.** `RechargeApiKey` flipped `Enabled=true` whenever post-top-up counters were under limit. That rule exists for auto-deactivation (`RecordApiKeyUsage` disables on exhaustion), but `Enabled=false` has TWO causes — exhaustion and a deliberate operator disable for abuse/chargeback/disputed order — and the entry records neither. So a banned buyer could restore their own access by paying again | `config/apikeys.go:290` | Over-limit state is now snapshotted BEFORE limits are raised; being over limit is the only evidence exhaustion caused the disable, so it is the sole license to re-enable. The limit increase still applies either way — declining to lift a quarantine must not discard paid-for allowance |

Same defect class as #39: an automated recovery path overriding an operator's
deliberate quarantine because the state it keys off is overloaded. #64 carries a
positive control proving a genuinely quota-exhausted key is still revived, so
"never re-enable" cannot pass as a fix while breaking the feature's purpose.

| # | Defect | Site | Notes |
|---|---|---|---|
| 65 | **`previous_response_id` was not scoped to the owning customer key — cross-tenant prompt disclosure.** The `/v1/responses` store had NO owner field (`storedResponseDoc` carried id/model/output/stored_input and no key identity), and the handler resolved a continuation with a bare `loadResponse(id)` filename lookup. Any customer key that learned another customer's response ID could replay it: `expandPreviousResponseHistory` expanded the victim's stored INPUT and assistant OUTPUT into the prompt forwarded upstream, and returned it as the attacker's own context. **Proven on real bytes** — the upstream payload captured in the RED test contained tenant A's `MY-PRIVATE-PROMPT-acquisition-price-is-42M` and `MY-PRIVATE-ANSWER-board-approved-the-deal` inside tenant B's `history[]` | `proxy/responses_handler.go:46`, `proxy/responses_store.go` | `OwnerApiKeyID` stamped at both save sites (stream + non-stream), persisted, and checked before expansion. Refused as **404, not 403** — a "wrong owner" reply would confirm the ID exists, turning the endpoint into an oracle for probing valid response IDs |

The precedent this violated is in the same package: `responseCacheKey` prefixes
`apiKeyID` specifically so one tenant's cached response can never be served to
another (`proxy/response_cache.go:142-147`), and round-4's `4466808` pinned that
end to end as "the most serious defect this cache could have". The persistent
responses store had no equivalent.

Empty owner is treated as unowned and stays readable, which preserves the
existing trust model exactly: this proxy runs `requireApiKey=false` by design
(§6c operator decision), so every caller is `""` there, and records written
before ownership existed keep working.

Class fix, not just the reported site: `collectAncestorChain`
(`proxy/responses_history.go:61`) was the second unchecked `loadResponse` and now
stops at the first foreign-owned hop. Scope stated honestly — with the handler
check in place a mixed-owner chain should not be constructible through the API,
so that layer is defense in depth rather than a proven-exploitable path.

Both fixes are RED-proven and discriminating: neutralizing the handler guard
(`if false && ...`) reproduces the leak with the victim's plaintext in the
upstream payload, and a positive control proves the OWNER can still continue its
own response, so "reject every `previous_response_id`" cannot pass as a fix while
destroying multi-turn.

Cumulative: **65 defects**.

### Round-9 — unreviewed files with no sibling test (`2723b7f`, `6911dcd`)

Targets picked for lacking a sibling `_test.go`: `auth/sso_token.go` and
`proxy/websearch_loop.go` (338 and 620 lines as audited; 378 and 691 after these
fixes).

Correction to how they were chosen: `auth/sso_token.go` genuinely had zero test
coverage, but `proxy/websearch_loop.go` did NOT — five other test files already
exercised its symbols (`websearch_test.go`, `websearch_max_uses_test.go`,
`websearch_multi_skip_test.go`, `websearch_loop_provider_guard_test.go`, and the
new billing test). A missing `<file>_test.go` is a weak proxy for "untested"; the
accounting defect below survived precisely because the existing five tests
covered content shape and provider guards, not billing.

| # | Defect | Site | Notes |
|---|---|---|---|
| 66 | **SSO device-flow errors echoed raw HTTP response bodies, leaking credentials to the API caller.** Five sites did `fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))`. Two of those endpoints return secrets: `/client/register` returns `clientSecret`, `/session/device` returns the device session `token`. Not log-only — the leak path was traced end to end: raw body → error string → the `errors` slice in `apiImportSsoToken` (`proxy/handler.go:5901`, called at `:5929`) → `strings.Join(errors, "; ")` → **JSON response body**. So any non-2xx during SSO import handed the caller the very credentials the flow was establishing | `auth/sso_token.go:100,131,178,214,248` | One `ssoFlowError(status, body)` helper. Keeps the OAuth `error` code when the body parses as JSON (bounded to 64 chars) and otherwise reports `HTTP %d` alone; the body is never echoed. RED-proven at all five: the test printed `clientSecret: "CLIENT-SECRET-MUST-NOT-REACH-THE-CALLER"` verbatim before the fix. Diagnostics deliberately verified to SURVIVE — over-redaction destroyed diagnostic markers twice earlier in this series, so a probe confirmed the result is still `HTTP 400: invalid_client_metadata` |
| 67 | **The web-search loop dropped every intermediate round's tokens and billed the whole request to one account.** Two accounting bugs in one block. (a) `inputTokens := round.inputTokens` read only the TERMINAL round while credits were accumulated with `totalCredits +=` — the disagreement between the two adjacent lines is what shows the token half was an oversight, not a design choice. A 2-round search consuming 500 then 700 input tokens reported 700. (b) All of it went to `lastAccountID`, which is overwritten each round, even though the pool's LRU (`GetNextForModelExcluding` advances `lastDispatchSeq` per dispatch) deliberately hands consecutive rounds to DIFFERENT accounts | `proxy/websearch_loop.go` | Per-round `webSearchAccountUsage` map; each account settles for the rounds it actually served, and `roundInputTokens` sums all rounds. Measured before/after on a real 2-round loop: `servedBy=[bill-B bill-C]` → B=500, C=756, uninvolved A=0, total 1256 = 1200 input + 56 estimated output |

#67's damage is not only the operator-facing per-account totals — those feed
quota-aware routing, which prefers the account with the most remaining quota, so
bad numbers flow straight back into routing decisions. The dropped-token half
lands on `recordSuccessForApiKey`, which drives `TokenLimit` auto-disable: a
token-budgeted customer key could overrun its budget by the whole of every
non-final round.

Two method notes worth keeping, both caught by neutralizing rather than by review:

- The first D1 assertion was a **false green**. It checked the POOL total, but
  per-account settling bills from a different variable (`perAccount[].inputTokens`),
  so reverting the fix left the pool total correct and the test still passed. It was
  re-pointed at the two surfaces the dropped value actually reaches — the customer
  key's `TokensUsed` and the client's reported `usage.input_tokens` — and now fails
  on both when neutralized (756 vs 1200, and 700 vs 1200).
- The first version of the attribution assertion passed **vacuously**: with two
  accounts, the MCP search between rounds consumes a dispatch slot and selection
  returned to the same account (`servedBy=[bill-A bill-A]`), so "no account billed
  for work it did not do" was never exercised. A third account makes consecutive
  rounds land on genuinely different accounts.

Selection order is deliberately NOT asserted — the picker is quota- and
health-aware, so pinning a schedule would encode a guess. The test records which
account served each round from the bearer token the proxy actually sent, and
asserts the invariant against that ground truth.

One production seam was added to make this provable: `mcpEndpointOverride`
(`proxy/websearch.go`), empty in production, following the existing
`kiroEndpoints` / `kiroHttpStore` convention. The multi-round loop cannot be
exercised end to end without it, and cross-round accounting bugs are otherwise
unprovable.

Cumulative: **67 defects**.

### Round-10 — the same region defect one layer deeper (`a1cf36f`)

Round 9's sibling fix (`7bb854f`) validated the explicit region on
`POST /admin/add_kiro_api_key`. It fixed the ROUTE, not the class: the shared
helper both other callers go through was still unguarded.

| # | Defect | Site | Notes |
|---|---|---|---|
| 68 | **`resolveApiKeyRegion` probed and returned a caller-supplied region without validating its shape.** `apiAddAccount` (`proxy/handler.go:4270`) forwards `account.Region` from the request body straight in, then writes the result to `account.Region` and persists it. A malformed label (`"bogus"`) was stored verbatim; `regionalizeURLForRegion` refuses that shape (`kiro_api.go:236`) and silently leaves traffic on us-east-1, so the stored bucket disagrees with where requests actually go. Because dedup keys on (UserId, region), the SAME upstream account can then be added again under us-east-1 — two pool slots for one Kiro account, doubling routing weight and double-counting quota in `/admin/pool` | `proxy/admin_bot_api.go:900` | Validate + normalize at the helper, not per-route. RED-proven: account persisted with `Region:"bogus"` at HTTP 200 |
| 69 | **The same call path stored a case-variant region unnormalized.** `EU-Central-1` was persisted as-is. Every other path lowercases before comparing (`regionalizeURLForRegion`, `kiroProfileRegionCandidates`, `validateRegionOverride` all `strings.ToLower` first), so the stored bucket never matches the one they compute — the same dedup-miss consequence as #68, reached without any malformed input | `proxy/admin_bot_api.go:900` | Same one-site fix; `validateRegionOverride` already lowercases, so returning `normalized` closes both. RED-proven separately |

Fixed at the shared chokepoint rather than at `apiAddAccount`, because the route
fix in `7bb854f` demonstrated the per-route approach leaves the class open. Both
callers are now covered by construction.

The second caller (`importKiroAPIKeyCredential`, `proxy/handler.go:6300`) discards
`validateRegionOverride`'s bool — `region, _ := ...`. Checked, NOT changed: a
malformed region there collapses to `""`, which routes into full region discovery
and yields a working region, so the outcome is benign (silently ignoring the
caller's stated region rather than erroring). Changing it would alter import
semantics — a product decision, not a defect fix. Recorded so it is not
re-discovered as new.

Method note: the positive control (`eu-central-1` still accepted and persisted)
was written and passing BEFORE the fix, which is what distinguishes this from
"reject every region" — a fix that would satisfy both failing tests while
breaking the supply path. Neutralization confirmed both new tests fail without
the fix (`targetRegions = []string{explicit}` restored → 2 fail, 3 pass).

Cumulative: **69 defects**.

### Round-11 — the fourth partial-stream site (`668fb85`)

Same shape as round 10: an earlier fix in this series closed three of four sites
in a class and the fourth was missed.

| # | Defect | Site | Notes |
|---|---|---|---|
| 70 | **A Converse stream on the OpenAI surface that broke AFTER client bytes was recorded as an account SUCCESS.** `c65c161` fixed exactly this in three places (`bedrock.go:358`, `bedrock_openai.go:744`, `bedrock_converse.go:788`) and missed the fourth. The code logged `"converse openai stream ended with error after partial output"` and then fell through to `recordBedrockSuccess` on the next line — it named the failure and billed it as a success. `pool.RecordSuccess` clears the account's error count and cooldown (`pool/account.go:818`), so an account throwing repeated mid-stream Converse exceptions cleared its own health on every one and stayed selectable; the request log also claimed success for a request the client saw fail, and tokens were metered from a truncated stream whose terminal `metadata` usage event never arrived | `proxy/bedrock_converse.go:882-891` | Now calls `oconv.finish()` then `recordBedrockPartialFailure`, matching `invokeBedrockOpenAIStream:743-745` exactly. All four sites verified converging on the same contract after the fix |

Why the existing test did not catch it: `bedrock_partial_failure_test.go` calls
`recordBedrockPartialFailure` **directly**. It pins the helper's behaviour, not
that any particular call site reaches it — so the one path that never called the
helper was invisible to it. The new test drives the real entrypoint
(`invokeBedrockConverseOpenAIStream`) over a hermetic `httptest` upstream that
emits two good frames then an exception frame, which is the only way the missed
branch is observable.

Test seam note: no new production seam was needed — `bedrockHTTPClientFor`
(already used by `bedrock_region_test.go`) and the existing `converseFrame` /
`buildFrame` helpers were enough to synthesize a genuine partial AWS
event-stream.

Two controls, both written before the fix and passing throughout: a complete
stream still records a success and still meters its tokens (so "always record a
failure" cannot pass), and a pre-first-byte failure still returns an error so the
dispatch loop can fail over (so "never fail over" cannot pass). Neutralization
confirmed the defect test fails without the fix while both controls stay green.

Cumulative: **70 defects**.

### Round-12 — the third event-stream reader (`91f981c`)

Third round running where the defect was found by asking whether an earlier fix
in this series covered every site in its class. This one is the highest-traffic
path in the proxy.

| # | Defect | Site | Notes |
|---|---|---|---|
| 71 | **An HTTP 200 whose body carried no event-stream frames was served to the customer as a billed success.** `parseEventStream` broke out of its loop on the first `io.EOF` and returned nil, so zero frames was indistinguishable from a stream that completed normally. `8f49a4b` fixed exactly this in BOTH Bedrock readers and left this third one — the reader serving the main Kiro path. On the streaming paths that nil is the only failover signal, so: no failover to a healthy account; `pool.RecordSuccess` CLEARS the offending account's error count and cooldown (`pool/account.go:818`) so it keeps looking healthy and keeps being selected; `inputTokens` falls back to `estimatedInputTokens` (`handler.go:2144-2146`) so the customer is billed a full input estimate for zero output; and the client receives `stop_reason: "end_turn"` as though the empty answer were real. The same nil also reaches the non-streaming path (`handler.go:2651`) | `proxy/kiro.go:745` | Mirrors `8f49a4b`: count frames, return `errKiroEmptyStream` on a clean EOF with none. A prelude that ARRIVES counts even when the loop skips it as malformed — a skipped frame still proves the upstream responded |

Classification was checked rather than assumed, because a new error on this path
could do more harm than the defect: the sentinel carries no HTTP status token, so
`pool.IsAuthFailure` returns false (no false ban, no auto-disable of a healthy
account) and `statusForUpstreamError` falls to its default 500 — the right answer
for an upstream that returned nothing.

Reconciling with a pre-existing test, rather than overriding it: 
`TestEventStreamHandlesUndersizedFrameLength` pins that a body consisting only of
an undersized frame parses cleanly and returns nil. Its stated intent is "no
panic, no hang", and a frame that arrived and was skipped is genuinely not an
empty stream — so the guard counts it, both tests hold, and neither assertion had
to be weakened. A fourth control (`TestKiroUndersizedFrameIsNotAnEmptyStream`)
now pins that distinction explicitly so a future change cannot collapse the two
cases.

Live-corpus evidence, offered as motivation and explicitly NOT as proof: across
three days, 2 of 16100 successful streaming responses recorded
`outputTokens: 0` with `ttfbMs` absent (0 of 667 comparable controls had it
absent), one billing 429252 input tokens and the other 137562, both against the
same account, both reported to the client as success. The corpus cannot establish
that those two were zero-FRAME rather than frames-carrying-no-text —
`markFirstByte` fires on emitted text, not per frame, and captured bodies store
only the request side. The defect is proven at the reader instead.

All three event-stream readers in the repo now carry the `framesSeen` guard.

Cumulative: **71 defects**.

#### Correction to the round-12 live-corpus paragraph (written after deploy)

The paragraph above overstated its evidence and is corrected here rather than
silently edited. The claimed signature — `outputTokens: 0` with `ttfbMs` absent —
is **not** discriminating: `markFirstByte` is only reachable from streaming code
paths, so `ttfbMs` is *structurally* absent on every NON-streaming row regardless
of health. The "0 of 667 comparable controls" figure only sampled streaming rows,
which is exactly what made a structural artifact look like a signal.

Measured after the round-12 deploy: of 82 successful rows, streaming 81/81 carry
`ttfbMs`, non-stream 1/1 does not — and that single matching row had 939 output
tokens, i.e. a perfectly healthy non-streaming response. The two historical rows
are therefore **unexplained, not evidence of this defect**; the corpus supplies no
live evidence either way. Defect 71 stands on the code-level proof at the reader,
which was always the stronger claim.

### Round-13 — client disconnects charged to the account (4 sites)

Found by asking the class question in the other direction: round 11/12 asked "did
an earlier fix reach every site?", this round asks "does a signal that all four
sites already handle actually MEAN what they assume?". It does not — the Bedrock
readers deliver events by writing to the client inside the reader callback, and
`readBedrockEventStream` propagates a callback error verbatim
(`bedrock_eventstream.go:53-57`), so a failed write to a departed CLIENT arrived
at all four callers as the same opaque `streamErr` as an upstream Bedrock fault.

| # | Defect | Site | Notes |
|---|---|---|---|
| 72 | **A customer hanging up mid-answer was charged to the serving account as an upstream failure.** The write error reached `recordBedrockPartialFailure` → `pool.RecordError`, so at 3 departing clients the account is cooled for a minute (`pool/account.go:878`) and at 5 its circuit breaker opens for 30s (`pool/account.go:149`). Operator error dashboards also blamed Bedrock for customers closing connections | `proxy/bedrock.go:339`, `proxy/bedrock_openai.go:588`, `proxy/bedrock_converse.go:766`, `:864` | Writes now tagged via `clientGone` / `writeAnthropicSSE`; `classifyBedrockStreamOutcome` routes client-gone to "record nothing" |
| 73 | **A disconnect on the FIRST chunk made one departed client penalise the whole pool.** With `started`/`streamedAny` still false the tagged error was returned to the dispatch loop, which sets `excluded[account.ID]` and calls `handleAccountFailure`, then retries the next account — whose write to the same dead socket fails identically, walking the pool and penalising every healthy account it touches (`handler.go:1741-1749`, `:2876-2882`) | same four sites | The client-gone check is evaluated BEFORE `started`, so a first-chunk disconnect is never mistaken for a pre-stream upstream fault |

The correct contract already existed in the sibling forwarder and was simply never
mirrored: `custom_api_forward.go:445-447` does `return nil // client gone; nothing
to fail over to` — no account penalty, no failover, no billing. `streamedAny` /
`c.started` is now also set only AFTER a successful write, so "the client received
bytes" can no longer be claimed for a chunk that failed to reach it.

Two RED-proven defect tests drive the real entrypoints (`invokeBedrockStream`,
`invokeBedrockOpenAIStream`) over a hermetic upstream through a
`failOnWriteRecorder` that returns `syscall.EPIPE`, and assert on the observable
consequence — the account's `ErrorCount` via `pool.DiagnosticsFor`, not an
internal call count. Four controls, written before the fix: a genuine upstream
partial failure still penalises the account, a pre-byte upstream failure still
fails over, a clean stream to a live client still succeeds and still meters
tokens, and a table test pins all four dispositions. Neutralizing `isClientGone`
to `return false` flips exactly the two defect tests plus the classifier unit test
and leaves all three behavioural controls green.

Cumulative: **73 defects**.

### Round-13b — the two holes round 13 left open (found by adversarial review)

An adversarial subagent review of the round-13 fix found two real gaps. Both share
one root cause: round 13 only inspected the return value of `Write` on the CONTENT
chunks, and that is not the only place a departed client is observable. Round 13
stopped over-PENALISING the account and left two paths that over-CREDIT it — the
mirror image of the defect it set out to fix.

| # | Defect | Site | Notes |
|---|---|---|---|
| 74 | **A disconnect during the TERMINAL chunk was still billed as a success.** `bedrockOpenAIStreamConv.finish` wrote the terminal `finish_reason`+usage chunk and the `[DONE]` sentinel and discarded BOTH `fmt.Fprintf` results, returning nothing. So a client that hung up after the last content chunk — or a stream whose only client write IS the terminal chunk — still reached `recordBedrockSuccess`: `successRequests` incremented, the customer key metered, `pool.RecordSuccess` called, for a response whose terminal frame never arrived | `proxy/bedrock_openai.go:687-704` (helper), call sites `:763` and `proxy/bedrock_converse.go:916` | `finish` now RETURNS a `clientGone`-tagged error; both success call sites classify it and record nothing. The two partial-failure call sites deliberately ignore it (`_ = conv.finish(...)`) — already recorded as failures |
| 75 | **A flush-time disconnect was invisible, so a stream nobody received was billed as a clean completion.** `net/http`'s `ResponseWriter.Write` succeeds into the connection's `bufio.Writer` while the socket error surfaces only on flush — and the stdlib DISCARDS it: `func (w *response) Flush() { w.FlushError() }`. Verified in the local go1.25.6 source, plus `ResponseController.Flush` preferring `FlushError() error`. Checking `Write` alone therefore cannot see a disconnect between chunks | all four write sites, via `proxy/bedrock_client_disconnect.go:98-114` | New `flushClient` helper uses `http.NewResponseController(rw).Flush()` and tags a failure as client-gone. `http.ErrNotSupported` is ignored (a writer with no flush is not a client fault); non-`ResponseWriter` writers fall back to the error-less `Flusher` so existing buffer-based unit tests are unaffected |

Reviewer questions closed as NON-ISSUES, each checked rather than assumed:
a genuine upstream error cannot be misclassified as client-gone, because
`converseStreamConv.process` / `finalize` return an `emit` error verbatim
(`return emit(ev)`, and `finalize`'s `if err := emit(delta); err != nil { return err }`)
— never wrapped, so `errors.Is` keeps the sentinel; and `resp.Body.Close()` is a
`defer` at every site, so returning nil early leaks nothing.

Two RED-proven defect tests plus three controls
(`proxy/bedrock_terminal_disconnect_test.go`). The fixes were neutralized
SEPARATELY, and the first attempt was inconclusive and is recorded as such: gating
out the whole `ResponseController` branch made two tests fail on their
PRECONDITION guard ("flush was never attempted") rather than on a billing
assertion. Re-neutralized correctly — still flush, discard the error — the flush
defect test then failed on its real assertion (`successRequests moved 0 -> 1`)
while its control passed. Neutralizing the `finish` check at both call sites failed
both terminal-disconnect tests on real billing assertions
(`totalTokens moved 0 -> 11` and `0 -> 16`) with all three controls green.

**Billing tradeoff, argued and decided.** On client-gone we now bill nothing, yet
Bedrock already consumed upstream tokens, so the operator pays for inference the
customer is not charged for — in principle a client could stream and hang up
repeatedly for free inference without ever touching its key's `TokenLimit`. It is
still the right default: (1) it matches the established `custom_api` precedent
(`custom_api_forward.go:445-447`) and consistency across providers matters more
than recovering a rare edge cost; (2) billing a response the customer never
received is the worse failure — it is a real overcharge on every accidental
disconnect, which is common, whereas the abuse case is deliberate and rare; and
(3) the abuse is bounded and observable, since the disconnect is logged per
account. If it is ever seen in practice, the fix is to meter tokens WITHOUT
`pool.RecordSuccess` (bill the customer, do not credit the account's health) —
recorded here so the option is not re-derived from scratch.

Cumulative: **75 defects**.

### Round-14 — /v1/responses had no Bedrock guard (found during roadmap research)

Found while inventorying the system for the research roadmap
(`docs/plans/ROADMAP_research_driven_upgrades.md`), and independently reported by
an API-surface research subagent. CLAUDE.md states the invariant plainly: "Bedrock
accounts must be excluded from every Kiro/AWS-SSO path (or they get 403'd and
auto-banned). If you add a new Kiro-facing loop, add an `IsBedrock()` guard."
`/v1/responses` is such a loop and had no guard.

| # | Defect | Site | Notes |
|---|---|---|---|
| 76 | **A Bedrock account selected on `/v1/responses` was dispatched to the KIRO endpoint.** Both loops branch on `IsCustomApi()` but never on `IsBedrock()`, so a Bedrock account fell through to `CallKiroAPIWithDiagnostics`. Bedrock accounts carry static IAM/API-key credentials and no Kiro OAuth material, so the call 403s and `handleAccountFailure` then penalises — and can permanently ban — a healthy Bedrock credential. Reachability was verified, not assumed: the pool has no `IsBedrock` filter, `accountHasModel` fails OPEN on a cold model list, and `ensureValidToken` early-returns nil for Bedrock, so nothing upstream stops the selection | `proxy/responses_handler.go:192-203` (non-stream), `:433-437` (stream) | Guard SKIPS rather than dispatches (`excluded` + `attempt--`, matching the sibling `IsCustomApi()` skip), because no OpenAI→Anthropic *Responses* translation exists yet. Claude/OpenAI surfaces branch INTO the Bedrock invoke path; Responses cannot |

**Latent, not live — stated precisely.** The live config has 24 accounts, all
`idc`/`external_idp`/`api_key`, and zero Bedrock accounts, so this could not fire
in the current deployment. It becomes live the moment one Bedrock account is added.

**A false green I had to throw away, recorded rather than hidden.** The first
version of the test grepped `responses_handler.go` for the string `IsBedrock()`.
It went red before the fix and green after, which looked like a valid RED-proof —
but neutralizing the guards with `if false && account.IsBedrock()` left BOTH tests
passing, because the *substring still matched*. A source-text assertion cannot
distinguish a live guard from a disabled one. Replaced with a behavioural test that
builds a Bedrock-only pool, points `kiroEndpoints` at an `httptest` server that
counts hits, and calls the real handlers: with the guards the Kiro endpoint is
never touched (0 hits, HTTP 503 "no accounts"), and under neutralization it fails
on the real observable — "a Bedrock account was dispatched to the KIRO endpoint 1
time(s)" — on both the stream and non-stream subtests, while the Kiro-account
control stays green.

Cumulative: **76 defects**.

### Round-15 — the pool lock nests cfgLock in three places (its own contract forbids it)

Found by following up a lock-order claim from a research subagent instead of taking
it on trust. The claim was partly right and partly wrong: the site it named is
real, but its stated consequence (deadlock) is not, and it missed two other sites.

The pool documents the rule itself at `pool/account.go:458-462`: read config
BEFORE acquiring `p.mu`, "so the pool lock never nests cfgLock". Commit `58727ec`
("hoist config reads above pool lock to avoid freeze under Save stall") hoisted the
reads that violated it — and left three behind.

| # | Defect | Site | Notes |
|---|---|---|---|
| 77 | **Three pool code paths call `config.*` while holding `p.mu`, parking the pool lock behind synchronous config-file I/O.** `cfgLock` is held for the WHOLE duration of a config write (`Save` → `atomicWriteConfig`: `os.ReadFile` + backup rotate + `CreateTemp` + write + fsync + `Rename` on the live 145,728-byte config). Any pool method that reads config under `p.mu` therefore blocks every other pool operation — dispatch, cooldown stamping, model-list updates — for the duration of a disk write it does not control | `pool/account.go:477` (`GetQuotaAwareRouting` under `p.mu.Lock`); `:1427` in `diagnosticsForLocked`, reached under `p.mu.RLock` from BOTH `DiagnosticsFor` (`:1346-1349`) and `ModelRoutingFor` (`:1358-1369`) | Fix hoists the read above the lock in all three: `quotaAware` is now read beside `allowOverUsage`, and `diagnosticsForLocked` takes `allowOverUsage` as a parameter instead of reading it |

**Correcting the subagent's claim rather than repeating it.** It reported this as a
lock-order/deadlock hazard. Verified it is a STALL, not a deadlock: package
`config` imports nothing from `kiro-go` (checked — zero `kiro-go/*` imports), and
`cfgLock` is unexported and referenced only inside `config`, so no reverse edge
`cfgLock → p.mu` exists and a cycle is impossible. The blast radius is
availability, not a hang. It also named only line 477 and missed
`diagnosticsForLocked` — the more reachable of the two, since it is hit by two
callers and by admin diagnostics traffic.

**Line 477 was the *less* severe site, for a non-obvious reason.** The hoisted
`allowOverUsage` read at `:463` sits above `p.mu` and blocks there first, so under
a stalled writer execution never reaches 477 with the lock held. The hoist narrows
the window; it does not close it — a writer that acquires `cfgLock` in the gap
between the two reads still catches 477 under the lock. Fixed for that residual
window, not for a reproducible freeze.

**RED-proof.** `pool/lock_order_stall_test.go` reproduces the mechanism instead of
asserting on source text. `config.Init` is pointed at a writer-less FIFO, so
`Load()`'s `os.ReadFile` parks holding `cfgLock` — the exact shape of a slow `Save`,
with no timing luck required. `SetModelList` is the canary: it takes `p.mu.Lock()`
and calls no config function at all (`:371-382`), so if it cannot proceed, another
pool operation is holding `p.mu`. Pre-fix both tests fail at 1.92s (frozen);
post-fix both pass at ~0.41s.

**A neutralization that proved nothing, and why.** The first attempt reverted only
`diagnosticsForLocked` and both tests still PASSED. That looked like a false green
but was not: the partial revert is *shielded* by the hoisted read in the caller,
which blocks before `p.mu` is ever taken, so the re-introduced in-lock read is
unreachable. Full neutralization — reverting all three sites to their exact
pre-fix shape — makes both tests fail at 1.92s again. Recorded because the
shielding is the same effect that makes line 477 hard to reproduce, and a partial
revert would have silently understated the test's power.

**Test-premise bug found and fixed in the harness itself.** The first RED run had
one test fail on its own premise assertion, not on the defect. Cause: `config.Init`
takes `cfgLock`, publishes `cfgPath`, RELEASES it, and only then calls `Load()`
which re-acquires — a real window in which a config read legitimately completes.
The premise check now polls until a probe actually blocks rather than probing once.

Cumulative: **77 defects**.

### Round-16 — a hard-capped account is parked by nothing (dead code was the fix)

Found by working the roadmap's own top-priority item and checking its premise
rather than trusting it. The roadmap's F-C claimed "the overage path applies no
cooldown"; that was right, but its supporting measurement was wrong and its
suggested fix was already sitting in the tree, unused.

| # | Defect | Site | Notes |
|---|---|---|---|
| 78 | **A 402/overage account gets no durable backoff, so the pool immediately re-dispatches into an upstream that already refused.** The overage branch called `disableAccountOverage` + `RecordError(id, false)`. Neither parks the account: the former only re-reads and persists the upstream `OverageStatus` snapshot — and returns early if that live fetch fails — while `RecordError(..., false)` files a 402 as a GENERIC failure, which needs THREE consecutive errors before it applies even a 1-minute cooldown. Meanwhile `pool.MarkOverLimit`, which implements exactly the right behaviour (1h backoff via `setCooldownIfLater`, stamped either side of `Reload`), had **zero non-test callers** | `proxy/account_failover.go:453-455` (branch); `pool/account.go:1194` (the dead routine) | Fix calls `MarkOverLimit` FIRST, before the status refresh. Ordering is load-bearing: fetch-first means a slow or failing `FetchOverageStatus` delays the backoff, and via its early return can skip it entirely |

**Shipped behaviour had drifted from the repo's own stated intent.** The design
spec at `docs/superpowers/specs/2026-06-28-auth-upstream-reliability-design.md`
specifies `402 -> pool.MarkOverLimit` in two places. The code did something else,
and the routine the spec names was dead. This was drift, not an oversight in
design.

**MEASURED consequence.** On the 21 correctly-tagged live 402-overage events:
**20 of 21 re-selected the SAME account within 60 s, median gap 0 s.**

**Correcting a number I published last pass.** The roadmap previously reported
"7 s min / 77 s median / 274 s max" for this population. Re-measured: median gap
is 0 s. The earlier figure was computed over gaps between cap *events* rather than
the interval to the next dispatch of the capped account, and understated the
problem. Corrected in the roadmap in the same pass, not silently.

**RED-proof.** `proxy/overage_backoff_test.go` — 6 tests, behavioural (a real
`Handler` + pool, a seeded two-account pool, `handleAccountFailure` called with the
live error string form, then `GetNextForModelExcluding` polled to see who gets
picked). Pre-fix the capped account is re-selected 2 of 4 times. Under full
neutralization exactly the 2 behavioural tests fail and all 4 controls
(classifier boundary, no-ban, single-account fallback still works, generic failure
does NOT get the overage backoff) stay green — so the tests discriminate the fix
rather than the file.

**R3 re-checked and still refuted.** The corpus holds 229 untagged
`HTTP 402 from Kiro IDE:` strings that would miss `isOverageErrorMessage` (which
requires a 402 digit-boundary token AND the word "overage"). All 229 are from
07-25, before `upstreamError` began injecting the tag (`proxy/kiro.go:427`); the
21 tagged ones are 07-27. Every live 402 classifies correctly.

**Not fixed, and stated plainly:** the backoff is a flat 1h. For a *monthly* cap
that is still far too short — the account will be retried roughly 700 more times
before the period resets. Sizing it from `NextResetDate` is the remaining half and
carries its own risk (a wrong reset date parks a healthy account for weeks), so it
is left as roadmap work rather than guessed at here.

Cumulative: **78 defects**.

---

### Round-17 — the completeness axis (no new defects; two absences closed)

Round 17 is a different KIND of round and the count reflects that. Rounds 1-16
asked "what does this code do wrong". Round 17 asked "what does it not do at all",
which is the user's standing ask: *check everything and propose all features to
fully complete and comprehensively upgrade kiro-go*. The audit output is
`docs/plans/PROPOSAL_comprehensive_upgrade.md` (Tracks A-D, every item tagged
CODE-VERIFIED / DONE / ALREADY SHIPPED).

**Cumulative stays 78.** Neither item below is filed as a defect: they are missing
capabilities, not incorrect behaviour. Inflating the defect count with them would
corrupt the one number this document exists to keep honest.

| Item | Absence | Closed by |
|---|---|---|
| N-1 | **No CI gate at all.** 963 test funcs and ~32k lines of test code, with nothing running them on push or PR. A failing test, a data race, or gofmt drift could merge green | `60fa604` — `.github/workflows/ci.yml`: build + vet + gofmt + `go test -race`, pinned Go 1.23 to match the Dockerfile builder |
| N-2 | **No graceful shutdown.** `main.go:109` called `srv.ListenAndServe()` and nothing else. MEASURED tree-wide before the change: `signal.Notify` = 0 occurrences, `.Shutdown(` = 0 occurrences. Every deploy / `docker compose stop` / Ctrl-C killed the process outright, severing in-flight SSE streams mid-token and dropping pending stats, prompt-cache and trace rows | `28cb891` — `proxy/shutdown.go` + `main.go` + `proxy/handler.go` |

**N-2 completed infrastructure that was already half-built.** `stopRefresh` and
`stopStatsSaver` had **four readers** — `backgroundRefresh` (`handler.go:527`),
`importWatchLoop` (`import_watcher.go:71`), `backgroundStatsSaver` (`:2223`),
`backgroundTracePrune` (`:2388`) — and **zero writers**. No `Close`/`Stop`/`Shutdown`
method existed on `Handler` at all, so those loops could only ever die with the
process. `promptCacheTracker` had a `Stop()` that nothing called.

Two design points that are load-bearing and easy to break later:

1. `Close()` runs **after** the drain, so requests that complete during shutdown are
   included in the final stats save.
2. `Close()` must survive the **169 bare `&Handler{...}` literals** in the test
   suite, which leave stop channels and caches nil — closing a nil channel panics.
   Idempotency uses a `closeState` struct wrapping `sync.Once` so the ZERO VALUE is
   usable; that is the reason no existing test literal had to be edited.

**A latent config-corruption hazard, found by my own test failing the wrong way.**
The first RED run did not fail an assertion — it segfaulted. `config.UpdateStats`
(`config.go:1865`) dereferences `cfg` with no nil guard, and `Close()` is the first
caller in the tree that can run before `Init` succeeds. The crash is the lesser
half. The worse half: `Save()` would then marshal a nil `cfg` to the 4-byte literal
`null` — verified empirically, `json.MarshalIndent(nil, ...)` returns `("null", nil)`
— which is NON-EMPTY and therefore passes `atomicWriteConfig`'s empty-write refusal,
**clobbering a real config file with `null`**. Guarded at the source.

This respects an existing convention rather than inventing one: 19 config *readers*
nil-guard `cfg`; writers historically did not, because every writer ran after a
successful `Init`. A shutdown hook is the first caller for which that assumption
does not hold. **Deliberately NOT counted as defect #79** — it was unreachable in
shipped code and became reachable only via the path added in this same commit.

**RED-proof, both items.**

- N-1: a CI gate can only be proven by making it fail. Four throwaway probe files,
  one per step (`go vet` type mismatch, gofmt drift, `t.Fatal`, build break), each
  confirmed to exit non-zero on its own step. Probes removed, tree restored.
- N-2: `proxy/shutdown_test.go`, 6 tests. Under neutralization (`Close` reduced to
  its pre-fix no-op) exactly the **3 behavioural** tests fail — both channel-selector
  tests hang to their 2 s deadline, the prompt-cache flush never lands — while all
  **3 controls** (idempotency, bare-literal survival, nil receiver) stay green.
  Restored byte-identical, sha verified.

**N-2 verified end-to-end, not only by unit test.** Built to `/tmp`, ran with an
isolated `CONFIG_PATH` on port 18099, confirmed `/healthz` served, sent a real
`SIGTERM`, and observed the full ordered sequence in the process log — *signal
received (up to 30s)* / *HTTP server drained cleanly* / *handler closed: background
loops stopped, state flushed* / *shutdown complete* — with **exit 0**. The live
`data/config.json` (24 real accounts) was verified **byte-identical** before and
after, since the e2e wrote only to its isolated path.

**Stated plainly as a compromise, not a claim:** `shutdownGrace = 30s`. SSE streams
here can run for minutes, so no realistic deadline guarantees completion, and
waiting forever would hang a deploy behind one slow client. Docker's default 10 s
SIGKILL timeout will usually cut it shorter anyway — `stop_grace_period` must be
raised in compose if a full drain actually matters.

**Two claims I wrote into `CLAUDE.md` earlier and refuted in this pass** (recorded
here so they are not re-litigated):

- "The OpenAI-compatible surface is not wired for Bedrock" — FALSE.
  `proxy/bedrock_openai.go` (794 lines) implements the bridge and is dispatched from
  `handleOpenAIStream` (`handler.go:2873`) and `handleOpenAINonStream` (`:3346`). The
  real gap is narrower: only `/v1/responses` is unserved for Bedrock.
- "Bedrock model aliases are guesswork; ListFoundationModels would remove it" —
  FALSE. `proxy/bedrock_discovery.go` (415 lines) already calls `/foundation-models`,
  filters to ACTIVE on-demand text models, merges inference profiles, and caches per
  account with a TTL. `resolveBedrockModelID` consults discovery BEFORE the static
  map, so the aliases are a last-resort fallback.

**Open, prioritised in the proposal** (measured where a number appears): A3
body-size ceilings — ~~9~~ **5** unguarded `io.ReadAll(r.Body)` sites on the
customer hot path, an unauthenticated memory-DoS surface (**closed in round 17c
below**; the "9" was my own miscount, corrected there); A4 admin brute-force
limiting; B1 in-flight quota accounting, the largest measured efficiency win, since
quota state is up to 30 min stale and produced 112 cap errors in 8 minutes on one
account; B3 latency-aware routing, 1.42x median spread controlled for prompt size;
D1 context propagation, 32 `http.NewRequest` vs 1 `http.NewRequestWithContext`.

Cumulative: **78 defects** (unchanged — round 17 closed absences, not defects).

---

### Round-17c — the customer hot path had no body ceiling (A3 + C-1)

Same class as round 17a/b: a missing capability, not incorrect behaviour, so the
defect count again stays put. What makes this one sharper than the other two is the
exposure. `authenticate()` returns `(nil, nil)` when `requireApiKey` is off
(`proxy/auth.go`) — the live posture, deliberately so — which means the four
customer handlers were reachable with no credential AND buffered an unbounded body
with a bare `io.ReadAll(r.Body)` before any parsing. One request could make the
process allocate without limit. `ReadTimeout: 60s` bounds *duration*, not *size*.

| Surface | Site | Was |
|---|---|---|
| `/v1/messages` | `handleClaudeMessagesInternal` `handler.go:1561` | unbounded |
| `/v1/messages/count_tokens` | `handleCountTokens` `handler.go:1519` | unbounded |
| `/v1/chat/completions` | `handleOpenAIChat` `handler.go:2771` | unbounded |
| `/v1/responses` | `handleOpenAIResponses` `responses_handler.go:22` | unbounded |
| `/auth/import-cli-json` (admin) | `apiImportCliJson` `handler.go:6787` | unbounded, unlike its 4 siblings |

**Correcting a number I published in the proposal last pass.** That document said
"13 `MaxBytesReader` sites vs **9** bare `io.ReadAll(r.Body)`" while the table
directly beneath it listed **five**. Re-measured per site: there are 9
`io.ReadAll(r.Body)` occurrences, but **4 already had a `MaxBytesReader` assignment
on the preceding line** (`apiImportCredentials:5997`, `apiPreviewCredentials:6612`,
`apiApplyCredentials:6641`, `apiPreviewCliJson:6719`). Unguarded count: **5**. The
table was right, the prose was wrong. This mattered practically — "9 unguarded"
would have sent the next reader to re-guard four already-correct sites. Corrected in
the proposal in the same pass, not silently.

**Fix.** New `config.GetMaxRequestBodyBytes()` (field `maxRequestBodyBytes`) and a
new `proxy/request_body_limit.go` helper wrapping `http.MaxBytesReader`, returning a
sentinel so each surface answers **413** in its own dialect with type
`request_too_large` — the same type `account_failover.go:115` already maps an
upstream 413 to. `apiImportCliJson` instead took the plain `1<<20` bound its own
preview half already had, because those two endpoints are one feature and should
refuse at the same size. Wrapping the reader rather than trusting `Content-Length`
follows the convention already documented in `admin_bot_api.go`: a chunked or lying
header cannot slip past a reader that counts bytes.

**The default is an inference from the corpus, and is labelled as one.** The corpus
CANNOT measure customer body size — it stores the rewritten *upstream* body,
truncated at `TraceMaxBodyBytes` (256 KiB: 19,647 of 23,417 stored bodies are
flagged truncated), and among non-truncated records the stored request is usually
empty (p50 = 0 B/token), so bytes-per-token is not calibratable from it. What it
does measure, across 22,855 billed requests: p99 prompt **779,709 tokens**, max
**903,947**. At a pessimistic 12 B/token (highest ratio on any non-truncated record)
the largest real request is ~10 MiB, so **32 MiB** leaves ~3x headroom. Rejection
rate at candidate caps on that corpus: 1 MiB → **32.0%** of real requests rejected,
2 MiB → **4.6%**, 4 MiB+ → **0%**. Worth recording because a cap picked by intuition
would plausibly have been 1 MiB, i.e. an outage. Floor clamp `minRequestBodyBytes`
(64 KiB) exists for the same reason in the other direction — the p50 prompt alone is
~150k tokens, so a typo'd tiny value would reject everything.

**RED-proof.** `proxy/request_body_limit_test.go` (5 tests) and
`config/request_body_limit_test.go` (2 tests). Neutralizing the proxy helper to its
pre-fix bare `io.ReadAll`: all **4 behavioural subtests fail**, all **3 controls**
(normal body still accepted, genuine read error still 400 not 413, nil body
tolerated) stay green. Neutralizing the config default/clamp: **both** config tests
fail with the exact expected messages. Both files restored byte-identical, sha
verified against a pre-neutralization hash.

**A test-design lesson, recorded because it nearly produced a false conclusion.**
The first RED run did not fail an assertion — it **SIGSEGV'd**. Pre-fix, the
oversized body is buffered and the handler proceeds to dispatch, where a bare test
`Handler` has a nil pool (`pool/account.go:471`). That panic aborted the entire test
binary, so the three control tests never executed and the neutralization output was
unreadable; it looked like a broken test rather than a proven defect. The assertion
now invokes the handler under a `recover()` that records a panic AS the failure,
since reaching dispatch at all is exactly the defect being proven. **A RED signal
that crashes the runner is not a usable RED signal** — re-run per-test in isolation
to confirm which side actually failed.

Also fixed a process error of mine in the same pass: the first restore attempt failed
because the backup `cp` had written to a different filename than the restore read
(`kirogo_a3_helper_backup.go` vs `kirogo_a3_helper_orig.go`). Recovered from the real
backup and confirmed by sha. Lesson: verify the backup EXISTS before neutralizing,
not after.

Gate: build / vet / gofmt clean, `go test ./... -race -count=1` green across
auth/config/pool/proxy. Test count measured per package, not asserted: **970**
top-level test funcs (was 963, +7 — config 72, pool 91, auth 54, proxy 753).

Cumulative: **78 defects** (unchanged — A3 and C-1 are hardening, not defects).

---

### Round-17d — the admin password accepted unlimited guesses (A4)

Same class again: a missing control, not wrong behaviour, so the defect count holds
at 78. Both admin gates already compared the shared secret in **constant time**,
which closes a timing oracle and does nothing whatsoever about volume. Nobody was
counting failures.

| Gate | Site | Guards |
|---|---|---|
| `authenticateAdminKey` | `admin_bot_api.go:64` | the 9 machine-integration routes (mint / delete / recharge keys, add accounts) |
| `handleAdminAPI` | `handler.go:3675` | all of `/admin/api/*` — **including `/admin/api/config/export`**, which returns raw `config.json` with refresh tokens and `ksk_` keys |

MEASURED before the change: zero occurrences of `rateLimiter`, `Admit`, `lockout`,
or `failedAttempt` in either file. The per-key `rateLimiter` exists but is wired to
*customer* keys only (`auth.go:100`).

**Correcting the scope of my own proposal.** The N-5 write-up named only
`authenticateAdminKey`. There are **two independent gates**, each with its own
`ConstantTimeCompare`, and the one the write-up missed is the higher-value target —
`/admin/api/config/export` hands over every stored credential. Fixing only the named
gate would have left the better door open. Both now share ONE throttle, deliberately:
separate counters would let an attacker spend the full budget twice by alternating
surfaces.

**Fix.** New `proxy/admin_bruteforce.go` — per-source-IP failure counter, lockout
doubling from 2 s to a 15-minute cap once a source passes 5 failures, 30-minute decay
so old typos never accumulate, and a bounded 4096-entry state map with LRU eviction
that **never evicts an active lockout**. Both gates check `Allow` *before* comparing
the secret (a locked-out source learns nothing), book `RecordFailure` on every
rejection including the no-password-configured case, and call `RecordSuccess` on a
valid password so an operator who mistyped twice is not locked out by their next
mistake.

Three decisions worth keeping:

1. **Keyed on `RemoteAddr` ONLY — never `X-Forwarded-For`.** Zero XFF handling exists
   in this tree and there is no trusted-proxy config to validate such a header
   against. On a direct connection those headers are attacker-supplied, so keying on
   them lets one source reset its own counter every request by varying a header. **A
   lockout the attacker controls is not a lockout.** The cost is stated in the code
   rather than hidden: behind a reverse proxy all requests share one apparent source,
   so failures aggregate and an attacker can lock real admins out of that address.
   That is the safer direction — lost admin access recovers in ≤15 min; an unlimited
   guess budget against a credential-export secret does not.
2. **Bounded state map.** The keys are attacker-controlled, so an unbounded map would
   make this defence its own memory-growth surface — precisely what round 17c closed
   on the request path. Refusing to evict live lockouts denies the obvious bypass
   (flood the map with fresh addresses to clear your own penalty).
3. **A nil throttle is tolerated** — auth still enforced, only the lockout skipped —
   so the bare `&Handler{...}` literals across the suite keep working. Same
   constraint round 17b hit with `Close()`.

**RED-proof.** `proxy/admin_bruteforce_test.go`, 10 tests. Neutralizing BOTH gates to
their pre-fix shape fails exactly the **4 behavioural** tests (bot-gate lockout,
api-gate lockout, cross-gate sharing, XFF-spoof resistance) while **every
pre-existing admin test keeps passing** — so the neutralization removed the new
capability and nothing else. Both files restored and verified byte-identical against
pre-neutralization backups.

**A control that survived neutralization, and what I did instead of assuming.**
`TestAdminLockoutIsPerSourceAddress` passes with AND without the fix — the exact
false-green shape this project treats as worthless, since a lockout that never fires
also never leaks across sources. Instead of trusting it, it was run against the
specific mutant it exists to catch: `adminAuthClientIP` collapsed to a single
constant key (a global lockout). It was the **only** test in the file that failed.
Mutant reverted from backup and confirmed absent from the tree by grep. **Lesson: for
a control that cannot fail under neutralization, mutate the thing it claims to
constrain — otherwise it is decoration.**

**Not built, stated plainly:** the proposal also suggested a webhook alert on repeated
failures. Skipped deliberately — the lockout is the security control; an alert is
observability and belongs with D2 rather than being bundled in here unproven.

Gate: build / vet / gofmt clean, `go test ./... -race -count=1` green across
auth/config/pool/proxy. Test count measured per package, not asserted: **980**
top-level test funcs (was 970, +10 — config 72, pool 91, auth 54, proxy 763).

Cumulative: **78 defects** (unchanged — A4 is a missing control, not a defect).

---

### Round-18 — the SECOND merge (`hian699` v1.2.8): resolving 7 red tests

Not a defect round. This is the conflict resolution for the second merge (see §1:
`MERGE_HEAD` `8a2dfc4`, 48 incoming commits, merge base `a2e3971`). Build and vet
were already green when this round started; **7 tests were failing**, and they had
three genuinely different causes. Recording them apart matters, because treating all
seven as "merge splices" would have produced three wrong fixes.

**Category 1 — real splices (3). Merge cut code; restoring it is the whole fix.**

1. `proxy/translator_truncate_test.go:219` — the assertion checked for the *sibling*
   test's marker `"FINAL: summarize"` while this test's own fixture appends
   `"FINAL question"`. It could only ever fail. Assertion corrected to the fixture.
2. `proxy/handler.go` (OpenAI mid-stream error path) — the merge kept the
   `recordFailureForApiKey` bookkeeping call and then `return`ed, **dropping the
   fork's client-facing terminator**: no error chunk, no `finish_reason`, no
   `data: [DONE]`. That is exactly the defect the fork had already closed — a client
   that had received partial content saw the connection stop and could not
   distinguish truncation from completion. Restored, with the `recordFailureForApiKey`
   call kept so the failure is not double-counted (mirrors the sibling Claude path at
   `handler.go:2448`).
3. `proxy/kiro_api_test.go` — fixture omitted `AuthMethod`, but
   `shouldProbeFallbackRegions` returns **false** for an empty `AuthMethod` with a
   non-empty `Region` (deliberately: only `external_idp`, `idc`-non-BuilderId, and
   region-less accounts probe fallbacks). So eu-central-1 was never probed and the
   test failed with "no available Kiro profile". The scenario it describes *is* the
   Azure-tenant case, so the fixture now sets `AuthMethod: "external_idp"`.

**Category 2 — test-order pollution, NOT a merge defect (1).**

`TestSecurityStatusReportsDefaultPassword` passed alone and failed in the suite.
`config.passwordOverride` is a package-level var holding the `ADMIN_PASSWORD`
override; `SetPassword` writes it and **`Init()` deliberately does not clear it**
(in production the override must survive a config reload). So any sibling calling
`config.SetPassword` — there are ~45 such call sites in `proxy/*_test.go` — leaks
its password in. Fixed in the test by restoring the precondition
(`config.SetPassword("")`), **not** by changing production behaviour. Worth
remembering: "passes alone, fails in suite" means shared global state, so look for
an unreset package-level var before suspecting the merge.

**Category 3 — a genuine, irreconcilable POLICY conflict (2 tests, 5 changed
assertions). This one deserves care.**

The two sides disagreed on what an AWS event-stream EXCEPTION frame means, and their
tests contradict each other directly — no implementation satisfies both:

| On an exception-only stream | Fork (4 tests) | Upstream (1 test) |
|---|---|---|
| `parseEventStream` returns | `nil` | non-nil error |
| `OnComplete` fires | yes, usage 123/45 | must not fire |

**Resolution: drain-then-error** — neither side verbatim. The frame is recorded, the
loop keeps draining and delivers every subsequent content frame, and the error is
returned at end-of-stream (`failureFrameErr`, `proxy/kiro.go`).

Why this and not the fork's `return nil` (which R2 in §4 chose, and which HANDOFF
§6b called blocked):

- The fork's stated fear was *killing live streams mid-answer*. Draining removes
  that fear completely — nothing aborts early, all text still reaches the client. So
  the fork's rationale is satisfied; only its `err == nil` bookkeeping conflicts.
- `return nil` is the **same failure class round 12 closed in this same function**
  (`errKiroEmptyStream`): a false success makes `pool.RecordSuccess` clear the error
  count and cooldown, and bills the customer key an estimated input total.
- The sibling Bedrock reader already returns an error for these exact headers
  (`bedrock_eventstream.go:121`, asserted by `bedrock_eventstream_test.go:89`).
  Silence on the Kiro path made one upstream condition visible on one surface and
  invisible on the other.
- Tiebreak: a false success is silent, mis-bills, and un-cools an account; a false
  failure is loud and recoverable by failover.

**Upstream's `OnComplete must not fire` rested on a false premise, and I checked
rather than assumed.** `OnComplete` is implemented at 5 sites
(`handler.go:2420`, `:3073`, `:3704`, `:3919`, `:8390`) and every one only assigns
token counters. Success is gated on the returned error — `pool.RecordSuccess` is
called by the handler when `err == nil`, never by the callback. Suppressing
`OnComplete` would therefore silently stop billing tokens a failure frame carried,
which is precisely the accounting hole `TestFailureFrameStillCountsUsage` exists to
pin. So `OnComplete` still fires; the error return is what denies the false success.
That test's assertion was inverted (5 assertions changed across
`kiro_exception_frame_test.go`, `kiro_failure_frame_test.go`, `kiro_test.go`), each
with a `POLICY CHANGE` comment naming what replaced it — the original requirement
(reporting, no truncation, usage counted) is still asserted in every case.

**`ThrottlingException` → quota, without upstream's collateral damage.** Upstream
classified it by adding `"throttl"` to `isQuotaErrorMessage`. That is **not viable
here**: it would also match `errBedrockThrottled`, whose entire purpose is a
per-model skip that must NOT escalate to an account-wide 1h cooldown — asserted by
`TestErrBedrockThrottledNotQuota`. Instead `upstreamFailureFrameError` appends a
literal `HTTP 429` token for throttle types only, which `isQuotaErrorMessage` matches
via `pool.HasStatusToken`. Other types are deliberately left unmapped: the 403 path
*disables* an account, so a mis-inference there costs an operator a working account.

**One production fix fell out of category 1's investigation.** The cross-region probe
path called `acceptRefreshedProfileArn` (persists `ProfileArn` only) and never
persisted `ApiRegion`, while the OAuth-refresh path did (`kiro_api.go:609-616`).
`kiroRegionForProfile` still derives the right region from the cached ARN at request
time, so this was latent, not broken — but the persisted account showed an empty
`ApiRegion`, so admin tooling reading it directly, or a next-session `Load()`, would
not see the pin. Now mirrored via `UpdateAccountProfileArnWithRegion`, non-fatal on
write failure.

**Mutation-verified — first green proves nothing.** Baseline confirmed green first,
then each fix reverted in isolation and restored with a SHA-256 comparison:

| Mutation | Result |
|---|---|
| `kiro.go`: return `nil` (fork's old policy) | RED — 4 fail |
| `handler.go`: drop the error chunk + `[DONE]` | RED — 2 fail |
| `kiro_api.go`: drop the `ApiRegion` pin | RED — 1 fail |

All three files restored byte-identical, and the gate re-run afterwards from a **cold
build cache** (`go clean -cache`) — a mutate/restore cycle can otherwise leave the
last mutant in the build output and make `--no-build`-style reuse lie.

Gate: build / vet / `gofmt -l` clean, **1328 passed / 0 failed** across 6 packages,
`-race` clean. 7 red → 0.

Cumulative: **78 defects** (unchanged — these are merge-resolution fixes, not newly
discovered production defects; the `ApiRegion` persistence gap was latent).

---

## 4. Remaining work

### R13-followup — Bedrock paths never emit a request trace (CANDIDATE, not fixed)

Found while verifying round 13 and deliberately left alone: it is PRE-EXISTING and
untouched by that change (`git diff` confirms `handler.go` was not modified in
round 13).

Both Bedrock dispatch branches call `tr.beginAttempt(account)` and then `return`
without ever calling `tr.endAttempt` or `h.emitTrace`
(`handler.go:1737-1752`, `:2872-2885`). The Bedrock accounting path instead uses
the LEGACY log helpers (`recordSuccessLog` / `recordFailureWithDetails` via
`bedrock.go:528`), so Bedrock traffic produces a request-log row but no structured
trace row: no `Attempts[]`, no `TTFBMs`, no `RequestID` correlation, and
`AttemptCount` is never populated for these requests.

Consequence is observability-only — billing, pool health, and failover are all
unaffected because those go through the legacy helpers. Worth noting that this is
also why the round-12 live-corpus reasoning was weak: the corpus's structured
fields simply do not exist for Bedrock requests.

Fixing it means routing the Bedrock branches through `emitTrace` like the Kiro
paths, which is a larger change than round 13 and should be its own round with its
own RED tests.

### R1 — Complete the merge — DONE
Committed as `6158c24`, a real two-parent merge (`99dda52` + upstream `ec4ba56`);
`MERGE_HEAD` cleared. 70 files, +13651/-850.

All 14 previously-conflicted paths were verified marker-free before staging, and
the full gate was re-run from the exact staged state.

PUSHED. `origin/harry` is at `910c773`, confirmed by re-fetch (0 ahead / 0 behind,
SHAs match). The merge commit `6158c24` is reachable from the remote tip.

### R2 — mid-stream error reporting — RESOLVED (extend, not collapse)

User chose to BUILD OUT the error path rather than collapse it to `end_turn`.
Two changes, landed in `7bfbbb0`.

**1. `stop_reason: "error"` is KEPT deliberately.** It is outside Anthropic's
documented enum (`end_turn` / `max_tokens` / `stop_sequence` / `tool_use`), and
that is the point: a mid-stream abort is not a completion. Reporting `end_turn`
would tell the client the message finished normally, so partial output would be
treated as the whole answer — silent truncation, which is strictly worse than an
unknown enum value a client can branch on.

**2. The error TYPE is now classified** (`claudeErrorTypeForStatus`,
`proxy/account_failover.go`). It was hardcoded to `api_error` for every mid-stream
failure, while the OpenAI stream classified the SAME error via
`errorTypeForOpenAIStatus`. That asymmetry is consequential because an Anthropic
consumer keys its retry policy off `error.type`: a rate limit reported as
`api_error` invites an immediate retry into an exhausted account instead of a
backoff, and a revoked credential reported as `api_error` looks transient so the
client retries forever instead of surfacing "re-authenticate". Every returned
value is a type Anthropic actually defines; anything unrecognised degrades to
`api_error` rather than inventing an enum member.

**3. The ROOT CAUSE is now observable** (`proxy/kiro.go`). `parseEventStream` read
only the `:event-type` header, so an AWS event-stream EXCEPTION frame — which
carries `:message-type: exception` plus `:exception-type` instead — matched no
dispatch case and was silently discarded. That is *why* a mid-stream failure could
only ever reach the handler as a transport error, classifying as HTTP 500 /
`api_error` no matter what the upstream said. The parser now reads both headers
and reports them via `KiroStreamCallback.OnUpstreamException`, reusing
`extractHeaderString` already proven on the Bedrock path.

**Observation, NOT termination — and this is a deliberate stop short.** Promoting
exception types to fatal would need to know which types Kiro actually emits and
whether some are benign. No real Kiro exception frame has been observed, and the
trace corpus cannot supply one: `noteResponseText` stores ASSEMBLED text, so frame
headers are destroyed before capture. Treating a frame as fatal on the strength of
the AWS spec alone risks killing live streams mid-answer on the path every request
uses — a worse failure than the classification imprecision it would fix. The
frames are now logged, so promoting specific types later is a small change made
against evidence.

**Honest scope limit:** because mid-stream failures currently surface as transport
errors, the new classifier will in practice usually still return `api_error` on
the streaming path. It is correct and unit-tested, and it helps wherever a
status-bearing error reaches that site, but it is narrower in effect than it looks.

**A false green worth recording.** The first end-to-end SSE test for this passed
IDENTICALLY with the fix and with the hardcoded version, because the existing
mid-stream harness dies with `unexpected EOF` → 500 → `api_error` either way. It
was caught only by neutralizing the fix and seeing the test still pass, then
deleted rather than left in the suite implying coverage it did not provide. The
three exception-frame tests ARE red-proven: with detection removed the reporting
test fails on its real assertion while both "must not change behaviour" tests
still pass.

Also checked and downgraded: `getSortedEndpoints` indexes `kiroEndpoints[0..2]`
unconditionally on the "auto" path. Briefly flagged as a production panic risk,
then verified `kiroEndpoints` is a package-level 3-element literal never
reassigned outside tests — test-only fragility, documented in the harness rather
than "fixed" in production.

### R3 — `toolResult.status` enum — RESOLVED, no change needed

Settled from REAL captured traffic, not inference. The trace facility was already
enabled (`traceCaptureMode = redacted`) with 13,636 captured bodies on disk, so
the "needs a live capture" blocker was self-imposed — the captures existed the
whole time.

What 4,000 scanned request bodies show:

```
bodies containing toolResults : 3788
keys inside toolResults       : content 5911, toolUseId 4233, text 4219, status 2189
distinct `status` VALUES sent : "success"  (2182 occurrences) — and nothing else
```

The corpus therefore CANNOT reveal the accepted enum, because we hardcode
`Status: "success"` at both construction sites (`proxy/translator.go:1022` and
`:1652`) and never send an error value. The traces show what WE send, not what
Kiro accepts.

So the existing design stands, and the comment at `translator.go:1010-1015`
already reasons it out correctly: the failure marker goes in `Content` (free
text, the same channel `toolResultImagePlaceholder` uses) precisely because an
unaccepted enum would 400 every failed tool turn. The signal is lossless and
recoverable — `markToolResultFailed` is idempotent, and three tests pin it
(`proxy/tool_result_error_test.go`: marked on error, not marked on success, not
marked when `is_error:false`).

The OpenAI route deliberately does not mark failures: the OpenAI wire format has
no `is_error` field on a tool message, so there is nothing to translate.

Reopening this needs upstream DOCUMENTATION of the accepted enum, not another
capture. Captures of our own traffic can never answer it.

### R4 — Audit the new upstream surfaces — DONE (see §3b)
`web_search` via Kiro MCP, Microsoft Enterprise SSO, and the session-affinity /
circuit-breaker / dispatch-seq code in `pool/account.go` were all reviewed by
three independent `gpt-5.6-sol-thinking` reviewers at max reasoning effort, then
every claim was re-derived against live bytes by the parent before any fix. 18
further defects found and fixed; the false alarms are listed so they are not
re-litigated.

### R6 — Unauthenticated surface: OPEN BY DESIGN (operator decision, closed)

Recorded because an earlier round of this checkpoint described the exposure
INCORRECTLY, and the wrong version is the kind of thing a future reader would
try to "fix".

**What I reported at first, and why it was wrong.** I described `/v1/stats` as an
unauthenticated route and framed it as an information-disclosure decision. Both
halves were wrong:

- `/v1/stats` DOES have a gate — `handler.go` calls `h.validateApiKey(r)` and
  returns 401 on failure. There is no missing route check.
- The gate is bypassed at the source. `authenticate()` (`proxy/auth.go`) opens
  with `if !config.IsApiKeyRequired() { return nil, nil }`. The live config has
  `requireApiKey = false` and ZERO api keys, so *every* customer route is open —
  not just stats.

Proven behaviourally against the running container:

```
GET  /v1/stats     no-auth -> HTTP 200
GET  /v1/models    no-auth -> HTTP 200
POST /v1/messages  no-auth -> HTTP 200   <- real inference, no credential
```

So the exposure was never disclosure-only; it was fleet spend. The framing
understated it.

**A wrong fix that was started and reverted.** On the strength of the bad
diagnosis I began adding a `StatsRequireAuth` config flag. That would have been
actively harmful: a second, narrower toggle overlapping the existing master
switch, "securing" one read-only endpoint while `/v1/messages` stayed open, and
implying the problem was handled. Reverted before commit; no trace in history.

**Operator decision (recorded verbatim in intent).** This proxy is INTERNAL. The
open posture is deliberate, and the trust boundary is the network, not the
application — port 8080 binds `0.0.0.0`, so reachability is controlled outside
this codebase. If auth is ever enabled, only `/v1/messages` needs it; the read
endpoints may stay open.

**Do NOT flip `requireApiKey` to true as a "hardening" change.** With zero API
keys defined it 401s every request, including whatever depends on this proxy —
an outage, not a fix. The only safe sequence is: mint a customer key, update the
clients, THEN enable the flag.

### R5 — Design tradeoffs raised by subagents (deliberately not actioned)
Not defects; they need a product decision, not a unilateral rewrite:
- Per-request `Save()` fsync serializes routing behind whole-file disk I/O.
- Several `config` setters mutate in memory then `return Save()` with no
  rollback, while sibling setters do roll back.
- `/v1/models` is unauthenticated and can drive account error counters.
- Admin gate now fails closed on empty password (fixed), but `config.SetPassword`
  remains unguarded.

---

## 5. Roadmap status (`docs/feature-roadmap.md`)

F1–F12 are marked SHIPPED (health score, quota-aware routing, external-usage
auto-action, capacity forecast, response cache, per-key RPM/TPM, webhook bus,
model matrix, Prometheus `/metrics`, per-model cost, PII redaction, anomaly
detection). The roadmap is a design record, not open work. No open roadmap items
were found; the remaining work in §4 is audit/merge hygiene plus the new
upstream surfaces.

---

## 6. Validation gate (run all of these before claiming done)

**This gate is now a script.** `scripts/verify.sh` runs every check below, prints a
pass/fail table, and exits non-zero if any fails:

```bash
./scripts/verify.sh            # 12 checks, ~1 min
./scripts/verify.sh --race     # add the race detector (minutes)
./scripts/verify.sh --list     # print the checks without running them
```

The checks it runs, and the manual equivalents:

```bash
go build ./...
go vet ./...
gofmt -l .                                # must print NOTHING
go test ./... -count=1
go test ./... -race -count=1              # --race only
node --check web/*.js                     # every JS file, not just app.js
# every web/locales/*.json must parse
# en.json vs zh.json leaf-key sets must match exactly (vi is coverage-only)
python3 -c 'import yaml,sys; yaml.safe_load(open("docker-compose.yml"))'
docker compose config >/dev/null           # Compose schema, not just YAML
git diff --check
```

gofmt is clean across the whole tree — both formerly-flagged files
(`proxy/usage_anomaly_test.go`, `proxy/customer_admin_api_test.go`) were
whitespace-only alignment and were fixed in passing, so `gofmt -l` should return
NOTHING. A non-empty result now means the current change introduced it.

**Every check in that script was mutation-proven**: a defect of the class it claims
to catch was introduced, the check was confirmed RED, then the file was restored and
verified byte-identical by SHA-256. A check that has never been seen to fail is not
evidence. Details in §7.

**Why the non-Go checks exist.** The `hian699` v1.2.8 merge produced a tree where
`go build`, `go vet` and `go test` were all green while `web/app.js` did not parse
(7 splices → the whole admin bundle dead in the browser) and `docker-compose.yml`
was invalid YAML (deploy path dead). `.github/workflows/ci.yml` runs Go steps only
(`ci.yml:47-85`), so **CI would have passed that tree too**. `go test` is not the
gate; `scripts/verify.sh` is.

---

## 7. Round 18 — tooling, tutorials, and the doc-vs-code audit

Round 18 is not a defect round. It closed the merge, built the three scripts §6
refers to, and then — while writing tutorials from measured output — found that four
doc surfaces instructed operators to do things this tree cannot do.

### 7a. The merge is committed

`e902ed3`, parents `9f0b943` + `8a2dfc4`. **49 ahead / 0 behind `origin/harry`;
nothing pushed.** Gate green at that commit: 1328 tests, `-race` clean,
build/vet/gofmt clean, 0 conflict markers.

### 7b. `docker-compose.yml` was invalid YAML (deploy path was dead)

Found by the new Compose check, not by any Go tool. A sequence item had landed inside
the `healthcheck:` mapping, so `docker compose config` failed outright — meaning the
entire deploy path was broken on a tree whose Go gate was green. Fixed; both the YAML
parse and `docker compose config` now pass.

### 7c. The three scripts, and how each was proven

| Script | What proves it works |
|---|---|
| `scripts/verify.sh` | all 12 checks mutation-proven RED, plus a negative control on `html-ids` (table below) |
| `scripts/dev.sh` | ran `--smoke` and `--seed --smoke`; real `data/config.json` SHA-256 **identical** before/after; of 98 non-empty secret values in the real config, **0** appear in the seeded copy; **0** seeded accounts enabled; account count preserved 41 → 41 |
| `scripts/deploy.sh` | preflight run green end-to-end; extracted the binary from the image by tag, positive control present, version value `1.2.8` found |

Mutation proof for the gate — defect introduced, check confirmed RED, file restored
and verified byte-identical by SHA-256:

| Check | Mutation | Result |
|---|---|---|
| build | undefined symbol | RED |
| vet | `fmt.Printf("%s", 42)` | RED |
| gofmt | valid but misformatted Go | RED |
| test | `t.Fatal` probe | RED |
| js-parse | duplicate `const` in `web/toast.js` | RED |
| html-structure | nest a `.tab-content` inside another (the real merge bug, reproduced) | RED |
| html-structure | drop one `</div>` | RED |
| html-ids | give two real elements the same id | RED |
| locale-json | malformed `vi.json` | RED |
| locale-symmetry | removed one key from `zh.json` | RED |
| compose yaml | sequence item inside `healthcheck:` (the real merge bug, reproduced) | RED |
| compose config | `ports:` as a scalar | RED |
| whitespace | trailing whitespace in a tracked file | RED |

Each positive mutation ran with its sibling check as a control (unaffected = PASS),
and `html-ids` additionally carries a **negative** control — see §7i for why it
needed one.

### 7d. A defect in my own probe, caught by its positive control

`scripts/deploy.sh` first reported `positive control MISSING
(listKiroProfilesInRegion)`. The symbol was in fact present — 3 string occurrences,
1 in `go tool nm`. The bug was the probe:

```
strings BIN | grep -q PAT     # under `set -o pipefail`: exits 141, not 0
```

`grep -q` stops at the first match, closing the pipe, which kills `strings` with
SIGPIPE; `pipefail` then fails the pipeline. Measured: `with pipefail rc=141`,
`without pipefail rc=0`, `grep -c rc=0 count=3`. Fixed by materializing `strings`
output to a file once and grepping the file.

Without the positive control this would have read as "the image is missing my code"
and sent the next session debugging the merge. **This is the argument for positive
controls, recorded because it actually happened rather than as advice.**

### 7e. Docs instructed operators to do impossible things

Found by auditing every documented env var against `os.Getenv` in source, then
checking the SSO port mechanism the docs describe.

**`LOOPBACK_HOST` is dead.** Theirs read it (`HEAD^2:auth/kiro_sso.go:153,302`); the
merge kept ours' `KIRO_SSO_CALLBACK_BIND` (`auth/kiro_sso.go:299`) and dropped
theirs' reader. `git grep LOOPBACK_HOST -- '*.go'` matches nothing. Yet
`DEPLOYMENT.md` called it **BẮT BUỘC** (mandatory) in Docker, and its Lỗi #1
troubleshooting entry told operators to `printenv LOOPBACK_HOST`. An operator
following that doc would set a variable with no effect, get a loopback-only callback,
fail Enterprise SSO login, and be sent to debug the wrong knob.

**The documented SSO port scan does not exist.** `DEPLOYMENT.md` §4 described binding
"the first free port" from a 10-port list via `kiroLoopbackPorts` / `bindKiroLoopback`.
Measured: `kiroLoopbackPorts` → **0 hits**; `bindKiroLoopback` → only a stale comment
in `auth/reuseaddr_windows.go:21`. Reality is a fixed constant
`kiroRedirectPort = "3128"` (`auth/kiro_sso.go:62`) and `startListener` binding
`addrs[0]` with no fallback. Compose publishes exactly two ports
(`${KIRO_PORT:-8080}:8080`, `127.0.0.1:3128:3128`), not the six the doc showed.
The error string the doc quoted ("tất cả port loopback đều bận") does not exist in Go
code; the real one is `cannot bind %s for the SSO callback`
(`auth/kiro_sso.go:314`).

**Two README headings had been glued onto the wrong bodies** by the merge:
`## Thinking Mode` sat above auth prose + the endpoints table, and
`## Environment Variables` sat above the thinking-mode/outbound-proxy sections —
leaving a duplicate `## Environment variables` further down. Renamed to match their
actual content; heading set is now unique (verified: 14 `##` headings, 0 duplicates).

**The env table was wrong in both directions.** Fixed after measuring each reader and
default in source: dropped the dead `LOOPBACK_HOST` row, removed a duplicate
`ADMIN_PASSWORD` row, and added 7 variables the code reads but the table omitted —
`KIRO_SSO_CALLBACK_BIND`, `KIRO_IDE_PROFILE`, `BEDROCK_MODEL_MAP`,
`CUSTOM_API_CREDITS_PER_1K_TOKENS`, `TRACE_CAPTURE_MODE`, `TRACE_CAPTURE_ACK_RISK`,
`KIRO_TRUSTED_PROXY_HOPS`.

Fixed in `README.md`, `DEPLOYMENT.md`, `README_VI.md`, `README_CN.md`. Gate green
after. `.kiro/specs/kiro-sso-external-idp-hardening/*` still describes the 10-port
design — deliberately left alone: those are historical design records of the incoming
feature's intent, not operator instructions.

### 7f. A correction to my own count, recorded rather than quietly fixed

I first reported **7 phantom env vars** (documented but never read). That was wrong,
and a control probe caught it: 5 of the 7 *are* read, through `envInt` / `envInt64` /
`envBool` helpers in `proxy/dos_guard.go:101-105`. My regex only matched
`os.Getenv("LITERAL")` and was blind to helper-wrapped reads. Only 2 were truly
unreferenced in Go, and one of those (`KIRO_AWS_SSO_CACHE_DIR`) is a legitimate
Compose-level variable (`docker-compose.yml:50`). The real count of dead documented
variables is **1**: `LOOPBACK_HOST`.

Lesson worth keeping: a grep for one call shape is not an audit of a behaviour. Add a
positive control (names known to be read) before trusting an absence.

### 7h. The admin panel had two dead tabs — FIXED

This started as the "9 duplicated element ids" item below and turned out to be the
smaller half of a worse defect. Measured with an `HTMLParser` balance check:

| File | Structural errors | Duplicate ids |
|---|---|---|
| ours (`e902ed3^1`) | **0** | **0** |
| theirs (`e902ed3^2`) | **0** | **0** |
| merged (`e902ed3`) | **3** | **9** |

Both parents were clean; only the merge was broken. `<select id="logsFilterSelect">`
opened at line 938 and was never closed — the Logs tab body was cut off mid-element
and the API Log tab spliced in on top, leaving
`<div id="tabApilog">` (940-983) and `<div id="tabConsole">` (1032-1067) **nested
inside** the hidden `<div id="tabLogs">` (933-1068).

**Consequence:** `.tab-content` is `display:none`, and the tab switcher
(`app.js:6955-6956`) hides every `.tab-content` then un-hides only the target. A
hidden parent hides its children whatever their own class says, so the **API Log and
Console tabs could never render at all**. `go build`, `go vet`, `go test` and
`node --check` were all green on that tree.

The duplicated ids were the second-order damage: both tabs carried their own copy of
the `logs*` control ids, and `$()` returns the first match, so `app.js` bound *both*
handler sets to the Logs tab's elements — e.g. `logsClearBtn` fired `clearApiLog`
(7042) **and** `clearLogs` (7082) on one click.

**Fix**, reconstructed from the parent sections rather than hand-edited (verified
`lost=0`: every line of the broken region existed in the parent union before
replacing it):

1. Rebuilt the region as four siblings — `tabLogs`, the trace drawer, `tabApilog`,
   `tabConsole`. Result: 0 structural errors, 0 unclosed tags, no overlap between any
   two tab regions.
2. Renamed the API-Log tab's 7 colliding ids to `apiLog*` and repointed its `app.js`
   bindings, so each feature owns its own controls.
3. `saveProxyBtn` appeared twice in Settings (both parents shipped one; the merge kept
   both). The advanced/fallback copy was inert — renamed to `saveProxyFallbackBtn` and
   wired to the same `saveProxyConfig` handler.
4. Dropped ours' duplicate rpm/tpm form fields. This was **not** cosmetic: ours'
   labels claimed hard rejection ("Windowed rate limit… returns HTTP 429") while
   `app.js:3927-3931` documents that these inputs carry the *throttle* ints
   `rpmLimit`/`tpmLimit`; the hard-reject variants are the separate
   `rpmLimitHard`/`tpmLimitHard` keys (`config.go:442-443`) which **no UI sends**.
   Ours' labels were therefore already lying about what the field did, and its i18n
   keys were missing from `vi.json`. Theirs' labels ("throttles by delaying, not
   rejecting" / "display-only") match the code and are translated in all three
   locales.

Verified after: 0 structural errors, 0 duplicate ids, every id `app.js` addresses
statically resolves 1:1, and the full gate green. The 142 `$()` targets absent from
the HTML are all authored by `app.js` itself in dynamic markup — checked against both
parents, none was lost by the merge.

### 7i. The gate could not see any of that — two checks added, both mutation-proven

`scripts/verify.sh` had no HTML check of any kind, which is exactly why §7h survived
a green gate. Added `html-structure` (balanced tags + no nested `.tab-content`) and
`html-ids` (no duplicated id), covering every `web/*.html`. The gate is now **12
checks**, and both new ones were proven against the real defect:

| Mutation | Result | Control |
|---|---|---|
| nest a `.tab-content` inside another (the actual merge bug) | RED | `html-ids` unaffected |
| drop one `</div>` | RED | — |
| give two real elements the same id | RED | `html-structure` unaffected |

**A false positive in my first implementation, and the control that now prevents its
return.** The id check began as a regex over raw file text and flagged
`web/index-legacy.html:2411/2415` — two branches of an `if/else` **inside
`<script>`** that build the same id, where only one ever runs. Not a duplicate DOM id
at all. Ids are now collected through `HTMLParser`, which hands script bodies to
`handle_data` as CDATA. The mutation suite includes a **negative** control that
injects exactly that shape and asserts the check stays **green**.

This is the same error class as §7f: a regex matching one syntactic shape was
mistaken for an audit of a behaviour. The general rule now recorded in
`docs/tutorials/02-verification-gate.md`: **a check that can only go red is also
broken** — pair every positive mutation with a negative control.

### 7g. Known-and-not-fixed

- **`rpmLimitHard` / `tpmLimitHard` have no UI at all.** They are real, enforced
  config fields (`config.go:442-443`, rejecting with HTTP 429 via `rateLimiter.Admit`)
  and they appear in the admin wire structs (`admin_apikeys.go:39,121,313`), but no
  input anywhere sends them and `app.js:3929` records that neither side ever did. So
  the hard limits are settable only by editing `config.json` directly. Pre-existing
  design gap, unchanged by this round; noted because §7h's field cleanup is adjacent
  to it and a later reader will otherwise assume the form covers both pairs.
- **`vi.json` covers 705/1152 keys.** Not gated (it would have been red from day
  one); reported as coverage. The 447-key gap is this fork's own features. Dropping
  ours' rpm/tpm labels in §7h orphaned three en/zh-only keys
  (`apiKeys.limitRpm`, `apiKeys.limitTpm`, `apiKeys.rateLimitHint`) — now referenced
  by nothing. Left in place: removing them touches en+zh symmetry and belongs in a
  locale-pruning pass, not here.
- **Nothing merged is deployed.** Container `kiro-go-kiro-go-1` is `Exited (0)`; the
  running image predates the merge. Rollback tags preserved and verified reachable:
  `rollback-8197e45d`, `rollback-2958ed77` (9 days), `rollback-71f4e867` (11 days).
  Never `docker image prune` here without reading that list.
- **`out/` holds 5 untracked scratch files** from this session (`auth_gate.txt`,
  `auth_state.txt`, `lc_check.txt`, `lc_fix.txt`, `stage_auth.txt`) and is **not**
  gitignored, so `git add -A` would sweep them in. Left in place rather than deleted
  (shared repo, and they are not mine to remove); stage explicit paths instead.

## 8. Round 18c — B1 in-flight quota reservation (PROPOSAL B1 / roadmap P0-1)

Closed the largest *measured* inefficiency in the proxy. Not a new capability: the
signal already existed on the hot path and was being discarded.

### 8a. The defect

`isQuotaBlocked` -> `isOverUsageLimit(acc)` compares `acc.UsageCurrent` against
`acc.UsageLimit`. `UsageCurrent` is written **only** by an upstream refresh
(`RefreshAccountInfo` -> `config.UpdateAccountInfo`), and background refresh runs on
a ~30-minute cycle. So the number every routing decision was made on could be half
an hour old, and no amount of traffic moved it.

Live evidence (already in this checkpoint's round-18 corpus): **112 HTTP 402
`MONTHLY_REQUEST_COUNT` errors in 8 minutes** on `thuquan-pham@tainguyenvibe.com.vi`
= **134 dispatches into an account the proxy had already been told was capped.**
Each one costs a request slot, a retry, and client latency to be told the same thing.

Meanwhile `UpdateStats` — which runs after *every* successful request — already
received the upstream's own per-request `credits` figure and filed it into
`Account.TotalCredits`, a **lifetime** counter used only for reporting. The
period-scoped gate never saw it.

### 8b. Why a delta on top of upstream, not a replacement

Measured against the 41 real accounts in `data/config.json` before writing any code:

| account | `usageCurrent` | `totalCredits` | delta |
|---|---|---|---|
| xuanan-nguyen@… | 3876 | 3874.98 | **1.0** |
| ducdung-vu@… | 3870 | 3870.88 | **0.9** |
| tongkhoogn95267@… | 1281 | 1279.42 | 1.6 |
| user.brandon.garcia@… | 4882 | 1957.60 | **2925** |
| noor.holmes@… | 2486 | *(none)* | **all of it** |

The top rows agree to ~1 unit, which is what **proves the two share a unit**:
agentic requests, ~1-2 per request. They are **not** tokens — `credits/1k_tokens` is
0.005-0.026 across the fleet, so treating `credits` as tokens would have
under-counted by ~100x and the gate would never have fired.

The bottom rows diverge hard the other way because the same Kiro account is also
driven by the Kiro IDE and other clients. Local credits are therefore a **lower
bound** on period usage, never the whole truth — so upstream stays authoritative and
the delta only adds what we know happened since it was captured.

### 8c. Reset by observation, not by hook

`Reload` compares each account's `UsageCurrent` against the value the delta is
relative to (`lastSeenUsage`) and zeroes the delta when it moves — a fresh upstream
figure already contains the credits we tracked, so keeping them would double-count
and park a healthy account.

This is deliberately **not** a reset hook on the refresh sites. Several places write
`UsageCurrent` (background refresh, the admin refresh endpoint, the api-key batch
importer, per-account probes), they live in a different package, and a new one added
later that forgot to call a hook would double-count for a full period. A value
comparison cannot be forgotten.

Baseline adoption on first sighting does **not** clear the delta: an account can
appear in a `Reload` (first one after restart, or on re-enable) while requests are
already in flight, and clearing would throw away real observed usage.

### 8d. Blast direction, and the one gate deliberately left alone

The delta can only ever make an account look **more** used, never less, so the worst
case is parking an account slightly early — cost: one failover to a healthy sibling.
The failure it removes is the opposite and much worse.

Five of the six `isQuotaBlocked` call sites moved to the in-flight-aware
`p.quotaBlocked`. The sixth — the membership filter in `Reload` (`account.go:295`) —
was **left on the upstream-only rule on purpose**: that one drops an account from the
pool entirely, and an estimate should be able to make routing *skip* an account, never
*evict* it. The two paths that matter most were the sticky-affinity selector (a bound
api key would otherwise re-pick the same capped account for the whole refresh window)
and `eligibleForRoute`, the quota-*aware* selector's own gate — leaving that one stale
would have meant the feature whose entire purpose is quota routing still picking
capped accounts.

`Diagnostics` moved too, so the operator surface agrees with routing; reporting an
account available while every routing path skips it sends an operator hunting a
phantom fault.

### 8e. Verification

- `pool/inflight_quota.go` (new, 6.9K), `pool/inflight_quota_test.go` (12 tests),
  `pool/account.go` (2 struct fields, `UpdateStats` hook, `Reload` sync, 5 gates).
- **5/5 mutants killed, each by a distinct test** — gate ignores delta, reset hook
  disabled, sign guard removed, prune removed, overage precedence dropped. Every
  mechanism is independently constrained; restore verified byte-identical (sha256)
  after each mutation.
- 7 of the 12 are controls that stay green under the gate mutation (headroom,
  no-limit-data, overage, allowOverUsage, prune, refresh-reset, race) — without them
  a gate that blocked *everything* would have looked like a working feature.
- `go build ./...`, `go vet ./...`, `gofmt -l` clean. Full suite **1340 passed**;
  `go test ./... -race -count=1` **1340 passed, 0 races**. `pool` run at `-count=3`
  (318) to check for order-dependence.

### 8f. A flake I introduced and then fixed

The diagnostics test passed in isolation and failed in the full suite with
`TempDir RemoveAll cleanup: directory not empty`. Not an assertion failure: it is the
trap the `AccountPool` struct comment already documents — `UpdateStats` persists
config in a **detached goroutine** tracked by `pendingWrites`, so a late `Save()`
re-created `config.json` inside the `t.TempDir` the harness was deleting. Every test
I wrote called `UpdateStats`; only the race test happened to drain. Fixed with
`newTestPoolDrained`, which registers `WaitForPendingWrites` as a cleanup **after**
`config.Init`'s so LIFO runs the drain before the directory is removed.

Recorded rather than quietly fixed because it is a reusable lesson about this
package: **any pool test that calls `UpdateStats` must drain `pendingWrites`.**

### 8g. Defect count — a judgment call, stated

Count moves **78 -> 79**. §4 precedent (A3, A4) says "hardening and missing controls
are not defects", and B1 sits on the line: nothing was *unimplemented*, but the
router demonstrably made wrong decisions from stale state and burned 134 real
dispatches doing it. That is observable misbehaviour with live evidence, not an
absence, so it is counted. Flagged explicitly so a later reader can disagree with the
classification without having to re-derive the reasoning.

## 9. Round 18d — B2 overage backoff sizing (and why the proposal's own spec was wrong)

Finished the other half of round 16. **The item as written in
`PROPOSAL_comprehensive_upgrade.md` would have made things worse**, so it shipped in a
clamped form. Recording the refutation rather than quietly shipping something
different from what the plan said.

### 9a. What the proposal asked for

> "B2. Cap-aware backoff sizing — finish round 16's other half: size the overage
> backoff from `NextResetDate` instead of a flat 1h."

Round 16 wired the 402/overage branch into `pool.MarkOverLimit`, which parks the
account for a flat hour. Sizing that from the upstream reset date sounds strictly
better: park until the quota actually resets.

### 9b. Why the literal form is worse than the flat hour — measured, not argued

Probed all 41 accounts in `data/config.json` (host clock 2026-08-08) **before**
writing code:

| finding | count | consequence of naive sizing |
|---|---|---|
| `nextResetDate` = 2026-08-01, i.e. **7 days in the PAST** | **26 / 41** | `time.Until` negative; `setCooldownIfLater` treats a past expiry as nothing to do → **parked for ZERO seconds**, strictly worse than 1h |
| `nextResetDate` = 2026-09-01, **+24 days** | 15 / 41 | one 402 parks the account for 24 days |

The past-dated majority is not a data bug: the field is only as fresh as the last
upstream refresh, and those 26 accounts are disabled so they never receive one.

Worse, the **only currently-enabled account** (`david_smith25452`, 10000/10000 — i.e.
exactly at its cap, so the most likely to 402) is in the +24-day group. The naive form
would have withheld the entire serving pool for 24 days on a single 402.

Two further reasons the field is a poor clock:

- It is **date-only** (`"2006-01-02"`), truncated from a unix timestamp in
  `kiro_api.go:1530`, so even a valid value carries up to 24h of error.
- **Nothing else in the repo does arithmetic on it.** `admin_fleet_forecast.go:76` and
  `admin_usage_audit.go:83` pass the string straight through for display; the real
  forecast math uses `UsageCurrent`/`UsageLimit`. This would have been the first
  consumer to treat it as a clock, which is exactly why its staleness had never bitten.

### 9c. What shipped instead

`pool/overage_backoff.go`. Use the reset date when it is parseable **and** in the
future, clamped into **[1h, 12h]**:

- **floor = 1h** preserves round 16's contract, and turns the 26/41 stale-date case
  into a no-op rather than a regression;
- **ceiling = 12h** bounds the damage from a stale or wrong date to half a day, and
  means a capped account is retried ~twice daily instead of 24 times.

This **strictly dominates** the previous behaviour: never shorter than the flat hour,
never longer than the ceiling.

Interpreting the date as midnight UTC is the deliberately conservative reading — if
the real reset is later that day we under-park and retry slightly early (cost: one
402), whereas over-parking withholds a healthy account.

### 9d. Why a multi-hour park is safe here

Two mechanisms stop it becoming lost capacity:

1. **`fallbackEarliestCooldown`** (`account.go:574`) already serves the account whose
   cooldown expires soonest when nothing healthy remains, so a fully-parked pool still
   answers instead of going dark. This predates B2 — it is why a longer backoff is
   affordable at all.
2. **A new release valve.** `releaseOnPeriodRollover` drops the cooldown (and the
   consecutive-error count) the moment the upstream figure shows the period actually
   reset — so a real reset frees the account immediately rather than after the ceiling.

The valve is wired into `syncUsageBaselines` (the B1 machinery from §8), which is
already the one place that observes `UsageCurrent` changing. A **drop** is what a
billing-period reset looks like from here; the check sits before the baseline is
overwritten, since that is what makes the drop visible.

Direction of the risk was chosen deliberately: a drop could in principle be a bad
upstream read rather than a real reset, but clearing a cooldown is the **recoverable**
mistake — the next request either succeeds or 402s and re-parks the account.
Withholding a healthy account for 12h is not equally recoverable. Only cooldowns and
the error count are cleared, never the circuit breaker: a period reset says nothing
about whether the upstream is answering.

### 9e. The control that stops the valve becoming a bug

Releasing on *any* usage change would resurrect the defect §8 just closed (rising
usage un-parking a capped account). `TestUsageIncreaseDoesNotReleaseOverageBackoff`
and `TestUnchangedUsageDoesNotReleaseOverageBackoff` pin the asymmetry, and mutating
the drop check to `if true` is killed by the first of them.

### 9f. Verification

- 17 tests in `pool/overage_backoff_sizing_test.go`.
- **6/6 mutants killed, each by a distinct test**: ignore the reset date entirely,
  past-date guard removed, ceiling removed, floor removed, release-on-any-change,
  valve removed. Both mutated files restored and sha256-verified byte-identical.
- `MarkOverLimit` reads config **above** `p.mu` (config under the pool lock nests
  `cfgLock` beneath it — the anti-pattern already fixed twice in this package), and
  looks the account up via `config.GetAccountByID` rather than `p.accounts`, because
  an over-limit account has usually already been filtered out of `p.accounts` by
  `Reload`'s quota gate.
- `scripts/verify.sh` green; full suite **1357 passed**; `-race` **1357 passed, 0
  races**; `pool` at `-count=3` (369) for order-dependence.

### 9g. Defect count

Unchanged at **79**. B2 is a sizing improvement to a control that already worked, not
a defect — same classification as §4's A3/A4 precedent. The refuted *proposal item* is
recorded above as a plan error, not as a code defect.

## 10. Round 18e — D1 passthrough trace rows (and the third proposal claim to be wrong)

Closed PROPOSAL D1. Like B2, **the item as written was wrong** — and this time the
error was in the opposite direction: it understated one half of the problem while
asserting something false about the other. Recorded rather than quietly shipping
something different from the plan.

### 10a. What the proposal claimed

> "D1. Bedrock paths emit no structured trace row — roadmap F-E / P1-4."

### 10b. That is false, and the truth is worse in one respect

A Bedrock request **does** produce a request-log row:
`recordBedrockSuccess` (`bedrock.go:528`) → `recordSuccessLog` (`handler.go:2771`) →
`appendRequestLog` (`:2784`). Verified by reading the call chain, not inferred.

The actual defects:

1. **The row is the legacy minimal shape.** `recordSuccessLog` fills only
   `{Time, Endpoint, Model, AccountID, ApiKeyID, Status, Tokens, Credits, Duration}`.
   Absent: `RequestID`, `Outcome`, `API`, `Stream`, `HTTPStatus`, `Attempts`,
   `AttemptCount`, the input/output token split, `CacheReadTokens`,
   `CacheWriteTokens`, `TTFBMs`, `StopReason`, `ResponseModel`, `ToolCallCount`,
   `Region`, `ProfileArn`, `UpstreamHost`, `AccountEmail`, `BodyRef` — i.e. every
   field the trace UI and CSV export were built to read.

2. **A recorder was allocated and then thrown away.** This is the part the proposal
   missed entirely. `tr := newTraceRecorder(...)` runs at `handler.go:2020/3001/3370/
   3847` and `att := tr.beginAttempt(account)` at `:2054/3008/3377/3854` — both
   BEFORE the passthrough branch, which then `return`s at `:2104/3051/3402/3878`
   without calling `endAttempt` or `emitTrace`.

   Two consequences, both operator-visible:
   - the row carries **no `RequestID`**, so it cannot be joined to anything;
   - a failover chain that **ends** on a passthrough account **discards the attempt
     history of every account that failed before it**. An operator debugging "why did
     this request take three hops" gets a row that claims one clean success.

3. **It is a class defect, not a Bedrock one.** `recordCustomApiSuccess`
   (`custom_api_forward.go:540`) funnelled into the same `recordSuccessLog`. The user
   was offered Bedrock-only vs the whole class and **chose the class**, so both
   passthroughs are fixed.

### 10c. Why this had to be a swap, not an addition

`emitTrace` also ends in `appendRequestLog` (`request_trace_recorder.go:384`). Adding
the rich emit while leaving `recordSuccessLog` in place would write **two rows per
request** — exactly the row-inflation `emitTrace`'s own doc comment says it exists to
remove ("a failover chain produced multiple rows for one request").

The swap is safe on counters, which is the part worth checking before believing it:
`recordSuccessLog` only appends a row, and `emitTrace` bumps counters **only** on
`outcomeError`. Success counters keep coming from `recordSuccessForApiKey` on the
serving path, so this changes row SHAPE and nothing else.

### 10d. Design: thread the recorder through `forwardParams`

`forwardParams` (`custom_api_forward.go:311`) gained `trace *traceRecorder` and
`attempt *traceAttempt`. Chosen over adding parameters because all nine construction
sites keep compiling, and the omission is visible at the call site — a future
passthrough that forgets them logs a thin row instead of silently losing the trace id.

New choke point `proxy/passthrough_trace.go`:
- `recordPassthroughTrace` — closes the attempt, notes usage, emits **one** row; falls
  back to the legacy thin row when no recorder was threaded (`bedrockTestReply`, admin
  probes, ~170 test literals), which is what made this safe to drop into eight call
  sites at once.
- `notePassthroughFailedAttempt` — closes a failed attempt **without** emitting, since
  the request is not over. Emitting there would produce one row per attempt.

### 10e. Cache tokens: reported only where they are measured

Native invoke parses `cache_read_input_tokens` / `cache_creation_input_tokens`
(`bedrockUsageTokens`), so `extractInputCacheTokens` / `extractNonStreamCacheTokens`
now feed the row, and `markFirstByte` supplies TTFB (recorded only after the write
**succeeds**, so it means "client had bytes", not "we tried").

Deliberately NOT emitted elsewhere: Converse's usage object carries only
`{inputTokens, outputTokens}` (`converseResponse.Usage:339-342`), and custom_api's
`parseUpstreamUsage` reads only prompt/completion totals. `RequestLog`'s own comment
says an explicit `cacheReadTokens: 0` asserts "caching was measured and did not fire" —
so emitting 0 there would have written a **false statement** into the log. Left unset.

Cache figures are a **subset** of `inputTokens`, never added: `totalInput()` already
sums fresh + cache-read + cache-write, so adding them again would double-bill. A test
pins this (`Tokens == 370` for a 350/20 request with 200 read + 100 write).

### 10f. Verification

- `proxy/passthrough_trace.go` (new), `proxy/passthrough_trace_test.go` (12 tests),
  `bedrock.go` (+2 extractors, cache-aware recorder, TTFB), `custom_api_forward.go`
  (+2 struct fields, swapped recorder), `handler.go` (8 sites),
  `responses_handler.go` (1 site).
- **7/7 mutants killed, each by a distinct test**: double row, always-legacy row, no
  `endAttempt` on success, row-per-failed-attempt, cache dropped, extractor swaps
  read/write, no legacy fallback. Both mutated files restored and sha256-verified.
- The row-count invariant is pinned from BOTH sides — `TestPassthroughEmitsExactlyOneRow`
  (not two) and `TestPassthroughWithoutTraceStillLogs` (not zero). Without the second,
  a refactor that dropped the emit entirely would have looked correct.
- `go build`/`go vet`/`gofmt` clean; `scripts/verify.sh` green; full suite **1369
  passed**; `-race` clean.

### 10g. Defect count

**79 → 80.** Unlike B2 (a sizing improvement to a working control), this is observable
wrong behaviour: rows that cannot be joined, and destroyed failover evidence on a
path the trace subsystem was explicitly built to cover. The *proposal claim* being
false is recorded above as a plan error, not counted as a code defect.

## 11. Round 18f — a failed /v1/responses request was counted TWICE (defect 81)

Found while scoping D1b (the websearch legacy-row gap), not by looking for it. Worth
recording because the repo had **already written down the rule this code broke**, and
because my first attempt to protect the fix was a false green.

### 11a. The defect

`proxy/responses_handler.go:752-753` (streaming `/v1/responses`, mid-stream failure):

```go
h.emitTrace(tr, outcomeError, statusForUpstreamError(err))
h.recordFailureForApiKey(apiKeyID, "openai", model, 0, err.Error(), startedAt)
```

Both increment the same two counters:
- `emitTrace` with `outcome == outcomeError` → `totalRequests++`, `failedRequests++`
  (`request_trace_recorder.go:362-365`);
- `recordFailureForApiKey` → `recordFailure()` → the same two
  (`handler.go:2632-2635`).

So **one** failed request advanced `totalRequests` and `failedRequests` by **two**.
Consequence: the dashboard's failure rate and total volume are both inflated for any
`/v1/responses` stream that dies after first byte, and `totalRequests` stops
reconciling against the log row count — the same class of irreconcilability the trace
subsystem was built to remove.

`handler.go`'s own note above `recordSuccessLog` states the rule verbatim: *"Do not
reintroduce them on a route that already emits a trace: that route would then log
twice and double-count totalRequests."* The rule was written; this path violated it.

### 11b. Fix

Split the helper rather than deleting a call:

- `recordFailureForApiKey` — unchanged behaviour (counts + attributes). Still correct
  for every path that does **not** emit a trace: the Claude/OpenAI tails and the
  websearch sites.
- `recordFailureAttribution` (new) — per-key usage + the flat request-log entry,
  **no** counter bump. For paths where `emitTrace(outcomeError)` already counted.

`responses_handler.go:752` now uses the attribution-only variant. No behaviour is lost:
the trace row, the per-key failure attribution, and the flat log entry all still
happen — exactly once each.

### 11c. The false green, and how it was caught

First protection attempt was three unit tests calling `recordFailureAttribution` /
`emitTrace` / `recordFailureForApiKey` **directly**. All green, and the mutation battery
looked convincing on two of three mutants — but the mutant that matters, **restoring the
original bug at the call site** (`responses_handler.go:752` calling the counting variant
again), **SURVIVED the entire 1374-test suite**.

That is a false green for the exact fix the tests existed to protect: helper-level tests
cannot see a wiring mistake at a call site.

Fixed by adding `proxy/responses_stream_counter_test.go`, which drives the real
`handleResponsesStream` through the existing `setupMidStreamFailureHandler` harness
(deterministic: one valid AWS event-stream frame, then a truncated one) and asserts the
counter **delta is exactly 1**. The call-site mutant is now killed by it.

Note the harness deliberately **asserts** rather than `t.Skipf`s when the failure path is
not reached — the two sibling tests in `responses_stream_termination_test.go` skip, and a
skipping test would have been just as vacuous as the helper-level ones.

**Class-level lesson (recorded in the skill):** when a fix is a *call-site* change, the
mutation must be applied at the call site, not only inside the helper. A helper-level
mutant that dies proves the helper works, not that it is wired correctly.

### 11d. Verification

- 5 new tests (3 helper-level + 2 through the real handler).
- **4/4 mutants killed by distinct tests**: call-site double-count restored (the one
  that previously survived), `emitTrace` dropped at the call site, attribution variant
  counts again, counting variant stops counting. Both mutated files restored and
  sha256-verified byte-identical.
- `go build`/`go vet`/`gofmt` clean; full suite **1374 passed**; `-race` clean.

### 11e. Also corrected: a comment my own D1 made stale

`handler.go:2744-2753` described custom_api and native Bedrock as "the subsystems that
have no trace-recorder wiring". Round 18e wired both, so the note was actively
misleading for the next reader. Rewritten to say what is true now: both go through
`recordPassthroughTrace`; the flat helpers survive for the untraced fallback and for the
**websearch pair** (D1b, still open); and the double-count warning now cites this real
incident plus the `recordFailureAttribution` remedy.

### 11f. Defect count

**80 → 81.**

## 12. Round 18g — D1d: the other half of D1 (passthrough *partial-failure* rows)

Found while fixing a caller inventory in the skill that my own D1 had made stale.
Updating that table forced me to enumerate the remaining `recordSuccessLog` /
`recordFailureWithDetails` callers, and the enumeration showed D1 had only fixed the
**success** path.

### 12a. The defect

D1 (round 18e) routed passthrough *successes* through `recordPassthroughTrace`. The
mid-stream *failure* path was left calling the flat helper:

- `bedrock.go` → `recordBedrockPartialFailure` → `recordFailureWithDetails`
- `custom_api_forward.go` (partial-stream branch) → `recordFailureWithDetails`

So a passthrough stream that died **after** the client already received bytes produced
the legacy thin row: no `RequestID`, no `Attempts`, no outcome/status/region — even
though `forwardParams` had been carrying the recorder since D1. The recorder was
allocated, an attempt was opened against the serving account, and then discarded — the
exact waste D1 was written to remove, surviving in the sibling branch.

Worth being precise about what was **not** wrong: this was *not* a double-count. Those
two paths never called `emitTrace`, so the counters moved exactly once. The bug was row
*shape* and lost attempt history, not arithmetic.

### 12b. Fix

One new choke point, `recordPassthroughPartialFailure` (`passthrough_trace.go`), and both
call sites now use it. It closes the attempt **with its cause** (so the row says which
account broke and why) and emits one terminal row; when no recorder was threaded it falls
back to `recordFailureWithDetails`, preserving pre-D1d behaviour for untraced callers.

`http.StatusOK` is reported deliberately: the response headers were written and flushed
with 200 before the stream broke, so 200 is what the client actually received. Deriving
502 from the error would describe a response nobody was sent.

Free coverage worth noting: `bedrock_converse.go` already routes its partial failures
through `recordBedrockPartialFailure`, so the Converse path was fixed by the same edit
without touching it.

### 12c. The design mistake, and what caught it

My first implementation paired `emitTrace(outcomeError)` with a **new** non-counting row
helper (`recordFailureDetailsRow`), reasoning by analogy with round 18f's
`recordFailureAttribution` split — keep the trace row, keep the flat row, count once.

That was wrong, and wrong in this file's own documented way: `emitTrace` **already**
appends a row *and* counts (`request_trace_recorder.go:349-365`). Pairing it with any row
writer produces **two rows per failure** — the row-inflation the header comment of
`passthrough_trace.go` says `emitTrace` exists to remove. I had re-created the D1 defect
while fixing its sibling.

`TestPassthroughPartialFailureEmitsRichRow` failed immediately on `got 2` rows, before
any of this reached a commit. The fix was to **delete**, not add: `emitTrace` alone, and
the speculative helper split reverted out of `handler.go` entirely.

**Class-level lesson (recorded in the skill):** 18f's remedy was a *split* because two
counting helpers collided; 18g's remedy is a *deletion* because one helper already does
both jobs. Reaching for the previous round's shape without re-reading what the target
helper does is how a fix re-introduces the defect it is fixing. Read the emitter, then
choose.

### 12d. The false green REPEATED — the 18f lesson was written down and still missed

This is the part of the round worth keeping. §11c ends with a class-level lesson, and it
is recorded in the skill: *when the fix is a call-site change, mutate the call site.*

I then wrote five tests that all drive `recordPassthroughPartialFailure` **directly**, and
ran a battery of 8 mutants. Six died. The two that mattered:

```
[SURVIVED] M1 bedrock CALL SITE reverted to legacy row
[SURVIVED] M2 custom_api CALL SITE reverted to legacy row
```

Reverting **either** call site to `recordFailureWithDetails` — i.e. undoing the entire
D1d fix — passed all 1379 tests. Exactly the defect-81 false green, one round later,
against a written-down rule.

Why the helper tests could not see it: the counter total is identical on both branches (1
either way, by design — see 12b), and the tests that *do* touch these call sites
(`bedrock_partial_failure_test.go`) construct `forwardParams` **without** a recorder, so
they take the untraced fallback and cannot distinguish the fix from the legacy call.

Fixed by two tests that drive the real callers:
- `TestBedrockPartialFailureCallSiteEmitsRichRow` — calls `recordBedrockPartialFailure`
  with a *traced* `forwardParams` and asserts `RequestID != ""` + `AttemptCount == 1`.
- `TestCustomApiPartialFailureCallSiteEmitsRichRow` — drives the real `streamUpstream`
  loop with a body that yields one SSE chunk then errors (non-EOF), asserts it returns
  `nil` (headers committed, no failover), that the client actually got bytes (so the test
  cannot vacuously pass on a no-output path), and that the row is rich.

**Sharpened lesson for the skill:** knowing the rule is not applying it. The operational
form is a *procedure*, not a maxim — after writing tests for a call-site fix, run the
mutant that reverts the call site and confirm it dies. And when an existing test touches
your call site, check which branch it takes: a test that passes `nil` where the fix reads
a recorder is not coverage of the fix.

### 12e. Verification

- **7 new tests**: 5 at the choke point (single rich row, the one-counter-bump invariant,
  untraced fallback still counting and logging, committed-200 status, nil-error guard) and
  2 at the call sites (12d).
- **Mutation battery 8/8 killed**, each by a distinct named test: both call-site reverts,
  double-row, attempt-not-closed, status-derived-from-error, untraced-fallback-dropped,
  nil-guard-dropped, outcome-success. Battery restored the tree and the post-run diff
  shows only the intended D1d edits.
- `go build` / `go vet` / `gofmt` clean; full suite **1381 passed**; full-repo `-race`
  **1381 passed in 6 packages** at HEAD `4be1377` (not just `proxy`+`pool` — see 12g).

### 12f. Defect count

**81 → 82.**

### 12h. Forward pointer

D1b — the last trace gap, the websearch pair — is round 18h, §13 below.

### 12g. Stale async results nearly became this round's evidence — TWICE

Two background `go test ./... -race` jobs, launched in earlier rounds, both completed
*after* 18g was already committed:

```
Go test: 1369 passed in 6 packages     # launched round 18e
Go test: 1374 passed in 6 packages     # launched round 18f
```

Both are **baselines from tests that no longer describe the tree**: 1369 predates 18f's 5
tests and 18g's 7; 1374 predates 18g's 7. Each had executed against a working tree that no
longer exists. Quoting either as verification of 18g would have understated coverage and,
worse, attributed a green race run to code it never compiled.

Two arrivals is the point: this is not a one-off oddity but the normal behaviour of a
long-running job in a repo that keeps moving. Expect more of them, and expect the number to
look plausible — 1369 and 1374 are both *close enough* to 1381 to pass a glance.

It also exposed a real gap rather than only a bookkeeping risk: the race run I had actually
done at this HEAD covered `./proxy/ ./pool/` only (1174 tests). Re-running full-repo at
`4be1377` gives **1381 passed in 6 packages**, matching the non-race total — so the gap is
now closed, but it was open while the docs claimed `-race` clean.

**Procedure:** re-run at the current HEAD before quoting any figure a background job hands
back, and put `git log --oneline -1` in the *same* command so the output carries the commit
it measured. An async number with no commit attached is not evidence.

## 13. Round 18h — D1b: the websearch pair, and the LAST trace gap

`proxy/websearch.go`, `proxy/websearch_loop.go`. Closes the gap D1 (18e) opened and D1d
(18g) narrowed: these two were the final direct callers of the legacy row writers.

### 13a. The defect

`websearch.go:700` and `websearch_loop.go:223` called `recordSuccessLog` directly, and
four failure sites called `recordFailureWithDetails`. So both web-search surfaces produced
the **legacy thin row** — no `RequestID` (unjoinable), no `Outcome`, no `Attempts`.

The loop case was the worst instance in the repo, and for a reason the other paths do not
have: one mixed web-search request spans up to `maxWebSearchRounds` upstream rounds, and
the pool's LRU deliberately hands consecutive rounds to **different** accounts. So a
single request routinely touches several accounts, and *none* of that history was recorded
anywhere. The per-account token/credit split was already correct (`pool.UpdateStats` in the
settle loop, fixed in an earlier round) — what was missing was who was tried, in what
order, and why any of them failed.

### 13b. Shape decision: ONE row per request (user's call)

Two options were put to the user:

| option | keeps one-request-one-row? | cost |
|---|---|---|
| one row/request, `Attempts[]` per round | **yes** | request-level `AccountID` must pick one account |
| one row per round | no | easier billing read, breaks the invariant 4 rounds were spent building |

User chose one-row/request. Request-level `AccountID` therefore comes from `emitTrace`'s
existing rule — the **last** attempt — which for this loop is the terminal round's account,
i.e. the one that produced the rendered output. `Attempts[]` carries every account tried
across every round, so nothing is lost by that choice.

### 13c. Two structural facts that made this NOT a copy of D1/D1d

Both were measured, and either one would have produced a broken fix if assumed away.

**1. The recorder must be CREATED, not threaded.** D1/D1d threaded an existing recorder
through `forwardParams`. That is impossible here: both web-search entrypoints are
dispatched from `handler.go:1902-1913`, which is **before** the first `newTraceRecorder` in
that function (`:1953`, the response-cache-hit path). There is no upstream recorder to
inherit, so `handleWebSearchRequest` and `runWebSearchLoop` each construct one.

**2. Attempts must be opened INSIDE the callee loops.** `performWebSearch`
(`websearch.go:482`) and `callUpstreamForWebSearch` (`websearch_loop.go:273`) are **callees
that select their own account** and return only the one that finally served. An attempt
opened by the caller would therefore have a ~0ms duration and would omit every account
that failed first — precisely the failover evidence the trace exists to carry. Both
functions took a `tr *traceRecorder` parameter (nil-safe) and open one attempt per account
they try.

A corollary worth stating because it is easy to get backwards: in
`callUpstreamForWebSearch` the attempt is opened **after** the Bedrock/custom_api
eligibility skip. Those accounts are *ineligible*, not broken, and no dispatch happens
against them, so recording an attempt would invent a failure that never occurred.

### 13d. EIGHT retry loops, not six — the skill was wrong and it pointed away from the work

The skill's trace reference has carried a heading "**SIX** retry loops, not one" since the
subsystem was built, listing `handler.go` ×4 and `responses_handler.go` ×2, with the rule
that every trace feature must be threaded through all of them.

Measured:

```
proxy/handler.go          2049, 3047, 3420, 3901
proxy/responses_handler.go 180, 529
proxy/websearch.go         482   <- performWebSearch
proxy/websearch_loop.go    273   <- callUpstreamForWebSearch
```

**Eight.** And the two missing ones are exactly the two D1b had to instrument, so the stale
count pointed *away* from the work. Corrected in the skill, with the `search_files` command
to re-derive it rather than a number to trust.

### 13e. My implementation was wrong, and a test caught it before commit

First implementation added the `tr` parameter to `callUpstreamForWebSearch` and a doc
comment describing per-round attempts — but never added the `beginAttempt`/`endAttempt`
calls inside its loop. Signature threaded, behaviour absent. The doc comment was a lie the
compiler could not catch.

`TestWebSearchLoopEmitsOneRichRowAcrossRounds` failed with `AttemptCount = 1 but the
request made 2 upstream rounds` — the single attempt came from the MCP search between
rounds, while both *upstream* rounds recorded nothing.

That assertion exists because of the §12d procedure: the test asserts the row shape a
caller would actually observe (`AttemptCount >= rounds`), not that a helper was invoked.
A test that only checked "row has a RequestID" would have passed with both upstream rounds
untraced.

### 13f. Then the battery caught the TESTS — a COUNT assertion does not constrain CONTENT

First battery run killed 8/10 and left two alive:

```
[SURVIVED] M9  beginAttempt(nil)     -> attempts lose account identity
[SURVIVED] M10 endAttempt(att, nil)  -> a FAILED round recorded as a success
```

Both survived for one reason: every loop assertion I had written checked `AttemptCount`,
and **both mutants leave the count exactly right**. M9 produces attempts that name no
account — so the row cannot say which credential misbehaved, and the request-level
`AccountID` that `emitTrace` derives from the last attempt goes empty. M10 reports a
reroute as a clean success, erasing the very failover evidence this subsystem exists for.

This is the third variant of the same class in three rounds, and it needed a *different*
remedy than the previous two:

| round | false green | remedy |
|---|---|---|
| 18f | helper tested, call site unwired | mutate the **call site** |
| 18g | same, plus a side-effect-neutral fix | assert row **shape**, not counters |
| 18h | records counted but not inspected | assert **per-record** identity + outcome |

Closed with `TestWebSearchLoopRecordsFailedRoundAttemptWithItsCause`, which drives a REAL
failover through `runWebSearchLoop` (first upstream call returns 500, the retry inside
`callUpstreamForWebSearch` succeeds) and asserts, per attempt, a non-empty `AccountID` and
at least one `Outcome == outcomeError` carrying a non-empty `Error` — while the
request-level `Outcome` stays `success`, because the retry did serve the request.

It also guards against vacuity: `if upstreamCalls < 2 { t.Fatalf(...) }`. Without that, the
test would pass on a single-attempt path and constrain nothing — the same trap as the
`t.Skipf` rule from §11c.

**Rule recorded in the skill:** for anything that accumulates records, assert per-record
identity and outcome, never the length of the slice.

### 13g. Verification

- **5 new tests** in `proxy/websearch_trace_test.go`, all driving the REAL entrypoints
  (`runWebSearchLoop`, `handleWebSearchRequest`): one rich row across a multi-round
  multi-account loop, the failover-attempt content test (§13f), round-failure counted once,
  pure-search rich row, pure-search failure counted once. Plus the two existing
  `callUpstreamForWebSearch` callers updated to pass `nil`, which also pins nil-safety.
- **Mutation battery 10/10 killed**, each by a distinct named test: 4 call-site reverts
  (both surfaces × success/failure), row inflation, per-round attempt not closed,
  MCP attempt not closed, round failure reported as success, attempt identity lost, failed
  attempt recorded as success. Battery restored the tree; post-run `git diff` shows only the
  intended D1b edits.
- `go build` / `go vet` / `gofmt` clean; full suite **1386 passed**; full-repo `-race`
  **1386 passed in 6 packages**; `scripts/verify.sh` **12/12 green**.

### 13h. Defect count

**82 → 83.**

### 13i. The trace subsystem is now complete

`recordSuccessLog` and `recordFailureWithDetails` have **no remaining production callers on
a traced path**. What is left is deliberate:

| writer | remaining callers | why |
|---|---|---|
| `recordSuccessLog` | `passthrough_trace.go:85` | the untraced fallback for callers with no recorder (admin probes, tests) |
| `recordFailureWithDetails` | `passthrough_trace.go` untraced branch | same |

Every client-facing surface — Claude stream/non-stream, OpenAI stream/non-stream,
`/v1/responses` stream/non-stream, Bedrock and custom_api passthroughs (success **and**
mid-stream failure), and both web-search surfaces — now emits exactly one rich row per
request through `emitTrace`. Defect #1 of the trace design doc (`RequestID` never assigned
⇒ unjoinable rows) and defect #2 (failover chain collapsed to one row, losing the cause of
the reroute) are closed everywhere, not just on the paths the original build covered.

## 14. Round 18i — F1 file split: `handler.go` 9,458 → 1,534 lines

Commits `f6ec8f5` (tranche 1) and `f96d699` (tranche 2). PROPOSAL F1 / N-8.

### 14a. Why this is two halves, and only one shipped

N-8 asks for two things: split the file, and replace the 91-arm `ServeHTTP` switch with a
route table. They are **different risk classes** and bundling them would have made the
result unverifiable:

- A file split inside one package **cannot change behaviour**. Move the bytes, let the
  compiler resolve the same identifiers, and equivalence is structural.
- Replacing the switch **changes route precedence**. The arms are ordered by specificity
  and the file already carries a warning comment about a route being silently shadowed by
  a more general arm above it. That needs an equivalence harness that walks the real route
  set and proves identical dispatch per path, *before* the switch is touched.

Only the split shipped. The routing half stays open with that requirement written down.

### 14b. Inventory first — and the name-based grouping I threw away

Measured before cutting: 9,458 lines, 241 top-level blocks, 214 functions, 4 functions over
200 lines (`handleClaudeStream` 557, `handleOpenAIStream` 493, `handleAdminAPI` 293,
`apiExportAccounts` 213).

A first pass bucketed decls by name pattern and dumped **121 of 242 into "other"** —
useless for planning. Replaced with brace-balance extents plus subject grouping, which is
what produced a usable 14-file plan.

Two traps found by measuring rather than assuming:

1. **The author's own section banner lies.** `// ==== 静态文件服务 ====` (static file
   serving) sits at line 8783, but only **2** of the 25 decls after it are static serving.
   The rest are admin APIs — proxy config, proxy pool, log level, thinking config, and
   `apiExportAccounts` (215 lines). Grouping by banner would have produced a
   `handler_static.go` full of admin endpoints. Group by subject, verify the banner.
2. **Duplicate bare names exist**: `const (` ×3 (anonymous blocks) and `Error` ×2 (methods
   on `importValidationError` and `importPersistError`). A plan keyed on bare names would
   have silently moved the wrong one, so the tool refuses any non-unique match and accepts
   receiver-qualified names (`importPersistError.Error`) instead.

Also checked and clear: no `//go:embed`, no build tags, no `func init()` — nothing pinned a
decl to a particular file.

### 14c. Verified as a MOVE, not merely as passing tests

The split ran through a script that reports its own invariants — which is a self-report,
so it proves nothing on its own. The independent check: strip the `package` clause and
import block from the pre-split file and from every resulting file, then compare line
**multisets**.

| tranche | pre-split code lines | resulting | identical | lost | invented |
|---|---|---|---|---|---|
| 1 (6 files + residue) | 8,794 | 8,794 | yes | 0 | 0 |
| 2 (8 files + residue) | 5,845 | 5,845 | yes | 0 | 0 |

Imports were recomputed per file by `goimports` (found at `$(go env GOPATH)/bin`, not on
`PATH`) rather than by hand — hand-maintaining 14 import blocks is precisely where this
kind of refactor breaks.

**A verification bug worth recording.** The first attempt at the tranche-1 proof read the
baseline via `git show HEAD:proxy/handler.go` through a tool wrapper and got **0 lines**
back — the known output-dropping quirk. It then compared 0 against 8,794 and reported
`multiset identical: False` with "8,794 lines added". The *verification* had failed, not
the split. Redirecting `git show` to a file on disk and re-reading it fixed the check and
confirmed the baseline sha256 matched the pre-split hash. When a check reports a
catastrophic difference, confirm the check can see its inputs before believing it.

### 14d. Result

`handler.go` **9,458 → 1,534**, into 14 files: `handler_claude.go` 1150,
`handler_openai.go` 890, `handler_models.go` 513, `handler_accounting.go` 331,
`handler_logstore.go` 153, `handler_token.go` 190, `handler_admin_accounts.go` 1344,
`handler_admin_import.go` 1149, `handler_admin_settings.go` 664,
`handler_admin_sso_kiro.go` 658, `handler_admin_sso_microsoft.go` 490,
`handler_admin_logs.go` 338, `handler_health.go` 90, `handler_web.go` 37.

Named `handler_admin_*`, not `admin_*`: twenty `admin_*.go` files already exist in the
package, and the prefix keeps the split's output distinguishable from them.

Residue in `handler.go` is deliberate and coherent — `ServeHTTP` + the 91-arm switch,
`handleAdminAPI` and the admin auth gate, `NewHandler`/`Shutdown` plus the four background
goroutines, the `Handler`/`RequestLog`/`AuditLog` types, and the anonymous `const` blocks.
Routing and lifecycle, which is what a file called `handler.go` should hold.

### 14e. Verification

- `go build` / `go vet` / `gofmt` clean after each tranche.
- Full suite **1386 passed** and full-repo `-race` **1386 passed in 6 packages** — byte-for
  byte the same counts as the pre-split run at `01d97a9`. A pure move should not change a
  single test outcome, and it did not.
- `scripts/verify.sh` **12/12 green**.
- Line-multiset equivalence per tranche (§14c), which is the assertion that actually
  distinguishes a move from a rewrite.

### 14f. Stale figures corrected in this document's own planning text

N-8's heading claimed **8,198** lines and the handoff said **8.2k**. Measured at execution:
**9,458** — the file grew ~1,260 lines while the item sat open. Both corrected, with the
drift called out rather than silently overwritten, because the same staleness pattern has
now produced wrong plans three times this programme (the SIX-vs-EIGHT retry loops in §13d,
the `NextResetDate` sizing in §9, and this).
