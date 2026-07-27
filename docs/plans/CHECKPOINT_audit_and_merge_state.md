# Checkpoint — audit state, merge state, remaining work

Purpose: a resumable record of where this project stands, written so a fresh
session (or a reviewer) can pick it up without re-deriving anything. Every
claim below is verified against the live tree by real command output; items I
could not verify are labelled as such rather than asserted.

Last verified: repo `harry` branch, working tree mid-merge (see §2).

---

## 1. Verified state of the tree

| Fact | Value | How verified |
|---|---|---|
| Branch | `harry` | `git rev-parse --abbrev-ref HEAD` |
| HEAD | `6158c24` (merge commit) | `git rev-parse HEAD` |
| Merge parents | `99dda52` + `ec4ba56` (2 parents) | `git rev-list --parents -n1 HEAD` |
| Merge in progress | NO — `.git/MERGE_HEAD` cleared | file absent |
| Conflict markers remaining | 0 across all tracked files | grep for `^<<<<<<<`/`^>>>>>>>` |
| Pushed to `origin/harry` | **NO** — local only | `git rev-list --count origin/harry..HEAD` |
| Build | clean | `go build ./...` |
| Test suite | **826 passed, 0 failed** | `go test ./config/ ./pool/ ./auth/ ./proxy/ -count=1` |
| `-race` | clean, 0 data races | `go test -race ./... -count=1` |
| `go vet` / `gofmt` | clean | `go vet ./...`, `gofmt -l` |

**Interpretation.** The merge is complete and committed locally. It arrived as
*resolved but unstaged* (git reported `U` for 14 paths with no conflict markers
in any of them — an earlier session resolved them in place without `git add`);
every path was re-verified marker-free, the full gate was re-run from the exact
staged state, and the merge was then committed as a real two-parent merge.

Nothing has been pushed. `harry` is ahead of `origin/harry`.

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
| 44 | **Regression I introduced.** `upstreamStatusFromMessage` selected by PATTERN ORDER, not string position. For `refresh failed: 401 {...\"trace\":\"HTTP 503 from edge\"}` it returned **503**, so fix #23's 5xx gate then refused to ban a genuinely revoked credential. The body outvoted the header | `proxy/account_failover.go` | Now returns the LEFTMOST match across all patterns — every formatter writes the authoritative status at the front |
| 45 | Same positional flaw in the pool sibling, reached differently: `hasUpstream5xxStatusToken` matched any bare 5xx token ANYWHERE, so `refresh failed: 400 {...\"upstream returned 500\"}` hit the 5xx gate and a revoked credential was read as a server outage | `pool/account.go` | Replaced with `firstUpstreamStatusToken` (leftmost 400–599, same boundary rule). The two classifiers now agree on all 18 real formatter strings — pinned by a permanent cross-package agreement test so they cannot silently diverge again |

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

---

## 4. Remaining work

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

```bash
gofmt -l ./config ./proxy ./pool ./auth   # expect only the 2 pre-existing flags
go build ./...
go vet ./...
go test ./config/ ./pool/ ./auth/ ./proxy/
go test -race ./config/ ./pool/ ./auth/ ./proxy/ -count=1
node --check web/app.js
# locale symmetry: en.json vs zh.json leaf-key sets must match exactly
git diff --check
```

gofmt is now clean across the whole tree — both formerly-flagged files
(`proxy/usage_anomaly_test.go`, `proxy/customer_admin_api_test.go`) were
whitespace-only alignment and were fixed in passing, so `gofmt -l` should return
NOTHING. A non-empty result now means the current change introduced it.
