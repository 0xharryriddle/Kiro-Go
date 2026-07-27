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
