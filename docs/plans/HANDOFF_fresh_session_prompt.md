# Kiro-Go — fresh-session handoff prompt

Paste this whole file as the opening message of the new session. Every fact below
was verified by command output at handoff time (2026-07-30, deployed source
`5360970`; re-check `git log -1` and the running image before trusting any SHA here).
Where something is unverified or unknown, it says so — do not upgrade those to
facts without checking.

> **STATE CHANGED SINCE THAT HANDOFF — read this first (round 18).**
>
> **The `hian699` v1.2.8 merge is now COMMITTED.** `MERGE_HEAD` is gone.
> - The merge commit is `e902ed3` "merge: resolve hian699 v1.2.8 into harry
>   (48 commits)", parents `9f0b943` (ours) + `8a2dfc4` (theirs, from
>   `https://github.com/hian699/Kiro-Go`), merge base `a2e3971`, version now `1.2.8`.
> - **HEAD has moved on since then** — several rounds landed on top of the merge.
>   At the last update HEAD = `a399d79` (N-7 Docker non-root + HEALTHCHECK) and the
>   branch was **52 ahead / 0 behind `origin/harry`**. Re-run
>   `git log -1 --format='%h %s'` and
>   `git rev-list --left-right --count origin/harry...HEAD` rather than trusting
>   either SHA here.
> - Everything from the merge onward is **local only**. `origin/harry` is still at
>   `9f0b943`. Nothing has been pushed; pushing needs the user's authorization.
> - Gate green at last run: **1340 tests, `-race` clean** (was 1328 at merge time),
>   build/vet/gofmt clean, 0 conflict markers.
>
> **Three scripts now exist and are the entry point for routine work** (each
> mutation- or run-proven, none writes `data/config.json`):
> `scripts/verify.sh` (12-check gate), `scripts/dev.sh` (throwaway-config local run),
> `scripts/deploy.sh` (build + verify image + rollback tag). Walkthroughs in
> `docs/tutorials/`. Read `docs/tutorials/02-verification-gate.md` before trusting
> `go test` alone — see the next paragraph for why.
>
> **`go test` is not the gate, and CI does not close the hole.** The merge left
> `web/app.js` unparseable (7 splices → whole admin bundle dead) and
> `docker-compose.yml` invalid YAML (deploy path dead) while `go build`, `go vet`
> and `go test` were all green. `.github/workflows/ci.yml` runs Go steps only, so CI
> would have passed that tree too. Both are fixed; `scripts/verify.sh` now gates JS
> parse, locale JSON, en/zh key symmetry, and Compose validity.
>
> **The container is NOT running** (§2 below is stale on this point):
> `kiro-go-kiro-go-1` is `Exited (0)`, and the last-built image predates the merge.
> Nothing merged has been deployed. `scripts/deploy.sh` (no flags) is preflight only.
>
> Also stale below: failure-frame handling is **no longer observation-only** (§6b) —
> read 6b before touching `proxy/kiro.go`.

---

## 1. Your task

Continue the Kiro-Go audit-and-harden effort. It is not "finish a feature"; it is
**find real defects, prove them, fix them, verify, deploy**. The previous sessions
closed 78 defects across seventeen rounds (13 + a 13b follow-up driven by an
adversarial review of round 13's own fix, then 14, 15, 16, and 17a/17b). There is
no deadline and no fixed list — work the highest-risk unreviewed surface, then the
next.

**Round 17 changed the shape of this work.** Rounds 1-16 were defect-driven: find a
bug, prove it, fix it. Round 17 added a *completeness* axis — a full-surface audit
of what the gateway does not do at all, written up in
`docs/plans/PROPOSAL_comprehensive_upgrade.md` (Tracks A-D, each item marked
CODE-VERIFIED, DONE, or ALREADY SHIPPED). Work that document alongside the defect
hunt; it is the answer to the user's standing ask ("check everything and propose
all features to fully complete and comprehensively upgrade kiro-go").

Repo: `/home/harry-riddle/dev/github.com/0xharryriddle/Kiro-Go`
Branch: `harry` (tracks `origin/harry`)

**Rounds 14 and 15 (most recent, read these first).**

Round 14 (`d731d86`) — defect #76: `/v1/responses` had no `IsBedrock()` guard in
either dispatch loop, so a Bedrock account could be sent to the Kiro endpoint,
403, and be penalised by `handleAccountFailure`. Both sibling surfaces already had
the guard; CLAUDE.md states the invariant. Latent today (0 Bedrock accounts in the
live config), live the moment one is added.

Round 15 — defect #77: three pool paths called `config.*` while holding `p.mu`
(`pool/account.go:477`, and `diagnosticsForLocked:1427` reached from both
`DiagnosticsFor` and `ModelRoutingFor`), which parks the pool lock behind a
synchronous 145KB config write. The pool documents the opposite rule at
`account.go:458-462`, and commit `58727ec` had hoisted the other reads but left
these. Verified to be a STALL, not a deadlock — `config` imports nothing from
`kiro-go`, so no cycle is possible.

**Two lessons from those rounds worth carrying forward.**

1. *Never assert on source text.* Round 14's first test grepped for the string
   `IsBedrock()`. It went red-then-green and looked valid, but neutralizing the
   guard with `if false && account.IsBedrock()` left it passing — the substring
   still matched. Behavioural tests only.
2. *A neutralization that passes may be shielded, not false-green.* Round 15's
   partial revert kept passing because a hoisted read blocks before the lock is
   taken, making the reverted line unreachable. Revert ALL sites to the exact
   pre-fix shape before concluding a test is weak.

---

## 2. Verified state at handoff

| Fact | Value |
|---|---|
| HEAD before this handoff-only sync | `5360970` (round 17d handoff sync) — check `git log -1` for the resulting doc commit |
| Remote | `origin/harry` identical (0 ahead / 0 behind) |
| Working tree | clean at commit time |
| Tests | 980 top-level test funcs at that handoff (config 72, pool 91, auth 54, proxy 763). **Now 1063** after the v1.2.8 merge — config 77, pool 91, auth 63, proxy 832; `go test ./... -count=1` reports **1328 passed** including subtests |
| `-race` | clean, 0 data races (`go test ./... -race -count=1`) |
| `go vet` / `gofmt` | clean tree-wide |
| CI | **now gated** — `.github/workflows/ci.yml` runs build + vet + gofmt + `-race` on push/PR to `main`/`master`/`dev`/`harry`. Go 1.23, matching the Dockerfile builder |
| Live container | **STALE — now NOT RUNNING.** `docker ps --filter name=kiro-go` returns no row (round 18). It *was* healthy at the 2026-07-30 handoff: Docker health `healthy`, failing streak 0, restart count 0, `/healthz` + `/v1/models` + `/admin` all HTTP 200 after a full healthcheck interval. Nothing has been deployed since, and the v1.2.8 merge is not built into any image |
| Deployed image built from | `5360970`; image `sha256:2958ed773ea8450d4b1edf9ecc89d8880200c73d509c4083c93bc2eea284e1fb`. Rounds 14-17 are deployed |
| Rollback | tag `kiro-go-kiro-go:rollback-71f4e867` -> image `sha256:71f4e867f1746743b6ba8126448c5e41932d43653af4cb7b723e43acf0350cad`; extracted binary SHA-256 `ea1c94aca6ce42068e8abbf1870f5a4180eb13264c19c2bae6c82af4b52b9e03` |

Recent commits (newest first):

```
5360970 docs: sync the handoff to cd53437 (round 17d)
cd53437 fix(admin): lock out brute-force guessing on both admin gates (round 17d, closes A4)
8419c59 docs: sync the handoff to d85d7de (round 17c)
d85d7de fix(limits): bound every customer request body (round 17c, closes A3 + C-1)
af16975 docs: record round 17 and sync the handoff to 28cb891
28cb891 fix(shutdown): drain in-flight requests and flush state on SIGTERM (round 17b)
60fa604 ci: add a build/vet/gofmt/test gate (round 17, closes N-1)
5e7b1ae docs: whole-surface completion & upgrade proposal (round 17 audit)
cf8dab7 fix(failover): park an over-quota account after a 402 overage (round 16)
82b74a9 docs: correct roadmap provenance after an unverified research batch
5872556 fix(pool): stop holding the pool lock across config reads (round 15)
d731d86 fix(responses): guard Bedrock accounts out of the Kiro dispatch loops
ec6784d docs: sync the handoff to 29e799c (round 13b)
29e799c fix(bedrock): do not bill a stream the client never received
f27d002 docs: record round 13 and sync the handoff to c013d52
c013d52 fix(bedrock): stop charging client disconnects to the serving account
bbc8814 docs: record round 12 and sync the handoff to 91f981c
91f981c fix(kiro): treat a Kiro event-stream with no frames as a failure
be20b4f docs: record round 11 and sync the handoff to 668fb85
668fb85 fix(bedrock): record a Converse OpenAI mid-stream failure as a failure
5c3667a docs: record round 10 and sync the handoff to a1cf36f
a1cf36f fix(admin): validate an explicit region before probing or persisting it
7ba344d docs: record round 9 and re-measure the unreviewed-file inventory
6911dcd fix(websearch): bill every search round to the account that served it
2723b7f fix(auth): stop echoing SSO device-flow response bodies into errors
5c748be test(tokens): pin the wire-vs-report estimator safety invariant
16a71ad fix(admin): bound request bodies on the admin-bot API surface
177f975 docs(checkpoint): record the unauthenticated surface as open by design
5b6fa90 fix(stream): stop labelling OpenAI client faults as server errors
5172d4b fix(stream): close three defects in the failure-frame observer
3242fff fix(stream): correct three Claude error-type mappings against the official enum
```

**Read `docs/plans/CHECKPOINT_audit_and_merge_state.md` first.** It is the
authoritative record: all 78 defects, the rejected claims (so they are not
re-litigated), and every deliberate non-decision with its reasoning.

Two entries there are worth reading before starting: the **correction to the
round-12 live-corpus paragraph** (a signature claimed as evidence turned out to be
a structural artifact of `ttfbMs` being streaming-only — the defect stands on its
code-level proof instead), and **R13-followup**, a pre-existing observability gap
where both Bedrock dispatch branches call `beginAttempt` but never `emitTrace`,
recorded as a candidate rather than fixed.

### Round 17 — the completeness axis (N-1, N-2 closed)

Round 17 audited what the gateway does **not** do, rather than what it does wrong.
Output: `docs/plans/PROPOSAL_comprehensive_upgrade.md`. Two items were closed in the
same pass; the rest are open and prioritised there.

**N-1 — no CI gate (`60fa604`).** The repo had 963 test funcs, 32k lines of test
code, and *nothing* running them on push. `.github/workflows/ci.yml` now gates
build + vet + gofmt + `go test -race`, pinned to Go 1.23 to match the Dockerfile
builder (`go.mod` still says 1.21 — the mismatch is real but deliberate, CI must
mirror what ships). RED-proven the only way a CI gate can be: four throwaway probe
files, one per step, each confirmed to make its own step exit non-zero. Probes
removed, tree restored.

**N-2 — no graceful shutdown (`28cb891`).** `main.go` called `ListenAndServe()` and
nothing else; measured tree-wide, `signal.Notify` = 0 and `.Shutdown(` = 0. Every
deploy severed in-flight SSE streams mid-token and dropped pending stats,
prompt-cache and trace rows. The infrastructure was half-built: `stopRefresh` and
`stopStatsSaver` had **four readers and zero writers**, and no `Close` method
existed on `Handler` at all.

Now: `ListenAndServe` on a goroutine under `signal.NotifyContext`, drained by
`srv.Shutdown` bounded by `shutdownGrace = 30s`, then a new `Handler.Close()`
(`proxy/shutdown.go`) that closes both channels, saves stats, flushes prompt cache
and trace store. Close runs *after* the drain so requests finishing during shutdown
still make the final stats save. A second signal restores default handling so an
operator can force-kill.

Two things worth carrying forward from it:

1. **`Close()` must tolerate the 169 bare `&Handler{...}` literals in the test
   suite**, which leave channels and caches nil. Closing a nil channel panics.
   Idempotency uses a `closeState` struct wrapping `sync.Once` so the *zero value*
   is usable — that choice is why no existing test literal needed editing.
2. **My own test caught a latent config-corruption bug in my own fix.** The first
   RED run segfaulted instead of asserting: `config.UpdateStats` (`config.go:1865`)
   dereferences `cfg` with no nil guard, and `Close()` is the first caller that can
   run before `Init` succeeds. Worse than the crash — `Save()` would then marshal a
   nil `cfg` to the 4-byte literal `null` (verified: `json.MarshalIndent` returns
   `("null", nil)`), which is non-empty and so passes `atomicWriteConfig`'s
   empty-write refusal, **clobbering a real config with `null`**. Guarded at the
   source. Deliberately NOT counted among the 78 defects: unreachable in shipped
   code, reachable only via the new path.

Verified end-to-end, not just by unit test: built to `/tmp`, ran on port 18099 with
an isolated `CONFIG_PATH`, sent a real `SIGTERM`, observed all four ordered log
stages and exit 0, and confirmed the live `data/config.json` (24 real accounts)
byte-identical before and after.

### Round 17c — no ceiling on customer request bodies (A3 + C-1, `d85d7de`)

Four customer handlers did a bare `io.ReadAll(r.Body)` with no size bound before any
parsing. What makes this sharper than a normal hardening item: `authenticate()`
returns `(nil, nil)` when `requireApiKey` is off (`proxy/auth.go`) — the live,
deliberate posture — so those handlers are reachable **with no credential**, and one
request could make the process allocate without limit. `ReadTimeout: 60s` bounds
duration, not size.

Closed by `config.GetMaxRequestBodyBytes()` (field `maxRequestBodyBytes`, default
32 MiB, clamped up from anything under 64 KiB) plus `proxy/request_body_limit.go`,
which wraps `http.MaxBytesReader` and returns a sentinel so each surface answers
**413 `request_too_large`** in its own error dialect. `apiImportCliJson` separately
took the plain 1 MiB bound its own preview half already had.

**A number in my own proposal, corrected.** It claimed "9 bare `io.ReadAll(r.Body)`"
while its own table listed five. Re-measured: 9 occurrences, but **4 already had a
`MaxBytesReader` on the preceding line** — unguarded count was **5**. Left in the
docs as a visible correction because "9 unguarded" would have sent the next reader to
re-guard four already-correct sites.

**The default is an inference, labelled as one.** The corpus cannot measure customer
body size: it stores the rewritten *upstream* body truncated at 256 KiB (19,647 of
23,417 stored bodies are flagged truncated), and non-truncated records are usually
empty (p50 = 0 B/token). What it does measure over 22,855 billed requests: p99 prompt
779,709 tokens, max 903,947 — so at a pessimistic 12 B/token the largest real request
is ~10 MiB. Rejection rate at candidate caps: **1 MiB would reject 32.0% of real
traffic**, 2 MiB 4.6%, 4 MiB+ 0%. Intuition would plausibly have picked 1 MiB, i.e.
an outage. That is why the floor clamp exists too.

**The lesson worth carrying forward: a RED signal that crashes the runner is not a
usable RED signal.** The first RED run SIGSEGV'd rather than asserting — pre-fix the
oversized body is buffered and dispatch proceeds, where a bare test `Handler` has a
nil pool (`pool/account.go:471`). That aborted the whole binary, so the three control
tests never ran and the neutralization output was unreadable. The assertion now runs
the handler under `recover()` and records a panic AS the failure, since reaching
dispatch at all is the defect. Re-run per-test in isolation when a RED run dies.

Also a process error of mine, recorded rather than hidden: the first restore failed
because the backup `cp` wrote a different filename than the restore read. Verify the
backup EXISTS before neutralizing, not after.

### Round 17d — the admin password accepted unlimited guesses (A4, `cd53437`)

Both admin gates compared the shared secret in **constant time** — which closes a
timing oracle and does nothing at all about volume. Nobody counted failures.
MEASURED before the change: zero occurrences of `rateLimiter`, `Admit`, `lockout` or
`failedAttempt` in either gate; the per-key `rateLimiter` is wired to *customer* keys
only (`auth.go:100`).

**There are TWO admin gates, and my own proposal only named one.** `N-5` described
`authenticateAdminKey` (`admin_bot_api.go:64`, the 9 machine-integration routes) and
missed `handleAdminAPI` (`handler.go:3675`), which gates all of `/admin/api/*`
**including `/admin/api/config/export`** — raw `config.json`, refresh tokens and
`ksk_` keys. That is the higher-value target of the two. Fixing only the named gate
would have left the better door open. Both now share ONE throttle deliberately:
separate counters would let an attacker spend the full budget twice by alternating
surfaces. **Lesson: when a doc names "the" auth path, grep for siblings before
believing it.**

`proxy/admin_bruteforce.go`: per-source-IP counter, lockout doubling from 2s to a
15-min cap past 5 failures, 30-min decay, bounded 4096-entry map that never evicts an
active lockout. `Allow` is checked *before* the secret comparison, so a locked-out
source learns nothing.

Three decisions to preserve:

1. **Keyed on `RemoteAddr` only — never `X-Forwarded-For`.** Zero XFF handling exists
   in this tree and there is no trusted-proxy config to validate one against. On a
   direct connection those headers are attacker-supplied, so keying on them lets a
   source reset its own counter every request. **A lockout the attacker controls is
   not a lockout.** Cost, stated in the code rather than hidden: behind a reverse
   proxy failures aggregate to one apparent source, so an attacker can lock real
   admins out of that address. Safer direction — admin access recovers in ≤15 min, an
   unlimited guess budget against credential export does not.
2. **The state map is bounded** because its keys are attacker-controlled; an
   unbounded map would recreate exactly the memory-growth surface round 17c closed.
   Never evicting a live lockout denies the bypass of flooding the map to clear your
   own penalty.
3. **A nil throttle is tolerated** (auth still enforced, lockout skipped) so the bare
   `&Handler{...}` literals across the suite keep working — same constraint 17b hit.

**The methodology lesson, and it is the important part of this round.**
`TestAdminLockoutIsPerSourceAddress` passes **with and without** the fix — the exact
false-green shape this project calls worthless. Rather than assume it was a fine
control, it was run against the specific mutant it exists to catch:
`adminAuthClientIP` collapsed to one constant key (a global lockout). It was the
**only** test in the file that failed, so it does constrain something real. Mutant
reverted from backup, confirmed absent by grep. **For a control that cannot fail
under neutralization, mutate the thing it claims to constrain — otherwise it is
decoration.**

Not built, deliberately: the proposal also floated a webhook alert on repeated
failures. The lockout is the security control; an alert is observability and belongs
with D2 rather than being bundled in unproven.

**Next items from the proposal, in priority order:** ~~B1 in-flight quota
accounting~~ **— DONE in round 18c, see the section below;** then B2 sizing the
overage backoff from `NextResetDate` (finishes round 16 honestly), F1 splitting the
8.2k-line `handler.go` (now safe to attempt, since CI guards it), B3 latency-aware
routing (1.42x measured median spread, controlled for prompt size — and now
unblocked, since it wanted B1's counter).

### Round 18c — the router decided on quota state up to 30 minutes stale (B1)

`pool/inflight_quota.go` (new) + 5 gate call sites in `pool/account.go`. Full
reasoning in `docs/plans/CHECKPOINT_audit_and_merge_state.md` §8.

**The defect.** The quota gate compared `acc.UsageCurrent` against `acc.UsageLimit`,
and `UsageCurrent` is written *only* by an upstream refresh on a ~30-minute cycle. No
amount of traffic moved it. Live evidence: 112 HTTP 402 cap errors in 8 minutes on
one account = 134 dispatches into an account already known capped.

**The signal already existed.** `UpdateStats` runs after every successful request and
already received the upstream's own per-request `credits` figure — it was filed into
`Account.TotalCredits` (a *lifetime* reporting counter) and never reached the
*period-scoped* gate. The fix records it as a delta and adds it on top of the last
upstream figure.

**Three things to know before touching this:**

1. **`credits` are agentic requests, not tokens** (~1-2 per request). Verified across
   41 live accounts: `credits/1k_tokens` is 0.005-0.026, so treating them as tokens
   under-counts ~100x and the gate never fires.
2. **The delta is added, never substituted.** `UsageCurrent` also includes usage from
   the Kiro IDE and other clients on the same account (one live account reads 2486
   upstream with *no* local counter at all), so local credits are a lower bound.
3. **Reset is by observation, not by hook.** `Reload` compares `UsageCurrent` against
   `lastSeenUsage` and zeroes the delta when it moves. Do **not** convert this to a
   reset hook on the refresh sites — there are several, they are in another package,
   and one that forgets to call it double-counts for a whole period.

**One gate deliberately left stale:** the membership filter in `Reload`
(`account.go:295`) still uses upstream-only `isQuotaBlocked`. That path *evicts* an
account from the pool; an estimate should be able to make routing skip an account,
never drop it. If you "fix" that for consistency, you are changing the blast radius.

**Verified:** 12 tests, 5/5 mutants killed by distinct tests (gate, reset hook, sign
guard, prune, overage precedence), 7 controls green under the gate mutation, full
suite + `-race` clean (1340).

**Trap this package will spring on you:** any pool test calling `UpdateStats` must
drain `pendingWrites` (config is persisted in a detached goroutine), or it passes
alone and fails in the full suite with `TempDir RemoveAll cleanup: directory not
empty`. Use `newTestPoolDrained`.

### Round 18d — overage backoff sizing, and a proposal item that was WRONG (B2)

`pool/overage_backoff.go` (new) + `MarkOverLimit` + a release valve in
`syncUsageBaselines`. Full reasoning in
`docs/plans/CHECKPOINT_audit_and_merge_state.md` §9.

**Read this before you ever size anything from `NextResetDate`.** The proposal said
"size the overage backoff from `NextResetDate` instead of a flat 1h". Measured on the
41 live accounts, that literal instruction is **worse than the flat hour**:

- **26/41 accounts carry a reset date 7 days in the PAST** (the field is only as fresh
  as the last upstream refresh, and disabled accounts never get one). `time.Until` is
  negative, `setCooldownIfLater` treats a past expiry as a no-op → the 402'd account
  would be parked for **zero seconds**.
- The other **15/41 sit 24 days out**, including the *only enabled* account
  (`david_smith25452`, 10000/10000 — the one most likely to 402). That is a 24-day
  pool-wide outage from one 402.
- The field is **date-only**, truncated from a unix ts (`kiro_api.go:1530`), so even a
  valid value is ±24h.
- **Nothing else in the repo does arithmetic on it** — forecast/audit surfaces pass the
  string through for display only. This was going to be its first use as a clock.

**Shipped instead:** use the date only when parseable AND future, clamped to
**[1h, 12h]**. Strictly dominates the old behaviour — never shorter than round 16's
hour, never longer than the ceiling.

**Why a long park is safe (do not "optimise" these away):**
1. `fallbackEarliestCooldown` (`account.go:574`) already serves the soonest-expiring
   account when nothing healthy is left, so a fully-parked pool answers instead of
   going dark.
2. `releaseOnPeriodRollover` drops the cooldown the moment `UsageCurrent` **drops**,
   which is what a period reset looks like from here. Wired into `syncUsageBaselines`
   (the B1 machinery), checked *before* the baseline is overwritten.

**The asymmetry is load-bearing:** release on a **drop** only. Releasing on any change
resurrects the defect B1 closed (rising usage un-parking a capped account). Two
controls pin this and the `if true` mutant is killed by them.

**Verified:** 17 tests, 6/6 mutants killed by distinct tests (ignore date, past-date
guard, ceiling, floor, release-on-any-change, valve removed); full suite + `-race`
1357, 0 races.

**Lock-order note:** `MarkOverLimit` reads config *above* `p.mu` and uses
`config.GetAccountByID`, not `p.accounts` — an over-limit account has usually already
been filtered out of `p.accounts` by `Reload`'s quota gate, so it would not be found
there.

### Round 18e — passthrough trace rows (D1), and the claim that was false

`proxy/passthrough_trace.go` (new) + 9 dispatch sites + both passthrough success
recorders. Full reasoning in `docs/plans/CHECKPOINT_audit_and_merge_state.md` §10.

**The proposal said "Bedrock paths emit no structured trace row". That is FALSE** — a
row was always written (`recordBedrockSuccess` → `recordSuccessLog` →
`appendRequestLog`). Read the call chain before re-deriving this. The real defects:

1. The row was the **legacy minimal shape** (9 fields). Missing everything the trace UI
   reads: `RequestID`, `Outcome`, `API`, `Stream`, `HTTPStatus`, `Attempts`, token
   split, cache tokens, `TTFBMs`, `StopReason`, `Region`, `ProfileArn`, `BodyRef`, …
2. **A recorder was already allocated and thrown away** — the part the proposal missed.
   `tr` at `handler.go:2020/3001/3370/3847`, `beginAttempt` at `:2054/3008/3377/3854`,
   then the passthrough branch `return`s at `:2104/3051/3402/3878` with no `endAttempt`
   and no `emitTrace`. So rows had **no `RequestID`** (unjoinable) and a failover chain
   ending on a passthrough **destroyed the attempt history of the accounts that failed
   first**.
3. **Class defect**: `recordCustomApiSuccess` shared it. Fixed for both passthroughs.

**THE TRAP if you touch this: it is a SWAP, not an addition.** `emitTrace` also ends in
`appendRequestLog` (`request_trace_recorder.go:384`). Calling both writes **two rows per
request**. `TestPassthroughEmitsExactlyOneRow` and `TestPassthroughWithoutTraceStillLogs`
pin the count from both sides (not two, not zero) — keep both or a refactor that drops
the emit entirely will look correct.

Counters are untouched by the swap: `recordSuccessLog` only appends, `emitTrace` bumps
counters **only** on `outcomeError`, and success counters still come from
`recordSuccessForApiKey`. This changes row SHAPE only.

**Cache tokens: only emit where measured.** Native invoke parses
`cache_read_input_tokens`/`cache_creation_input_tokens`. Converse carries only
`{inputTokens, outputTokens}` (`converseResponse.Usage:339`) and custom_api only
prompt/completion totals — those leave the fields **unset**, because `RequestLog`'s own
comment says an explicit `0` asserts "caching was measured and did not fire", i.e.
emitting 0 there would write a false statement. Cache figures are a **subset** of
`inputTokens` (`totalInput()` already sums all three) — never add them on top.

New callers: thread `trace: tr, attempt: att` into `forwardParams` and call
`h.notePassthroughFailedAttempt(...)` on the failure branch. Both are nil-safe, so an
untraced caller (`bedrockTestReply`, tests) falls back to the legacy row.

**Verified:** 12 tests, 7/7 mutants killed by distinct tests; full suite 1369 + `-race`
clean.

### Round 18f — a failed `/v1/responses` stream was counted twice (defect 81)

`proxy/handler.go` (+`recordFailureAttribution`), `proxy/responses_handler.go:752`.
Full reasoning in `docs/plans/CHECKPOINT_audit_and_merge_state.md` §11.

Streaming `/v1/responses` called `emitTrace(outcomeError)` **and**
`recordFailureForApiKey` on the same mid-stream failure. Both bump `totalRequests` and
`failedRequests` (`request_trace_recorder.go:362-365` and `handler.go`'s
`recordFailure`), so **one** failed request advanced both counters by **two** — inflating
the dashboard failure rate and breaking reconciliation against the log rows.

`handler.go`'s own note already stated the rule ("that route would then log twice and
double-count totalRequests"). Written down, and still violated. **Check the rule against
the code, not just the comment.**

Fix: split `recordFailureAttribution` (per-key attribution + flat log row, **no** counter
bump) out of `recordFailureForApiKey` (unchanged: counts + attributes). Use the
attribution-only variant wherever `emitTrace(outcomeError)` also runs; the counting
variant everywhere else (Claude/OpenAI tails, websearch).

**THE LESSON — a false green I shipped into the mutation battery and then caught.** My
first protection was three tests calling the helpers **directly**. All green. But the
mutant that matters — restoring the bug **at the call site** (`responses_handler.go:752`
using the counting variant again) — **survived all 1374 tests**. Helper-level tests
cannot see a wiring mistake at a call site.

Fixed with `proxy/responses_stream_counter_test.go`, which drives the real
`handleResponsesStream` via `setupMidStreamFailureHandler` and asserts the counter
**delta is exactly 1**. It **asserts** rather than `t.Skipf`s when the failure path is
not reached — the two sibling tests in `responses_stream_termination_test.go` skip, and a
skipping test is as vacuous as a helper-level one.

**Generalise it: when the fix is a call-site change, mutate the call site.** A helper
mutant that dies proves the helper works, not that it is wired correctly.

Also corrected here: `handler.go:2744-2753` described custom_api and Bedrock as having
"no trace-recorder wiring" — stale as of round 18e (my own change). Now says what is
true, and points at D1b for the websearch pair that genuinely still lacks it.

**Verified:** 5 tests, 4/4 mutants killed by distinct tests; full suite 1374 + `-race`
clean.

### Round 18g — D1d: the passthrough *partial-failure* row (and the SAME false green, again)

`proxy/passthrough_trace.go` (+`recordPassthroughPartialFailure`), `proxy/bedrock.go`,
`proxy/custom_api_forward.go`. Full reasoning in the checkpoint §12.

D1 (18e) only fixed the passthrough **success** path. A Bedrock or custom_api stream that
broke **after** the client already had bytes still went through `recordFailureWithDetails`
— the legacy thin row, no `RequestID`, no attempt history — while `forwardParams` had been
carrying a recorder since 18e. Not a double-count (those paths never called `emitTrace`);
the bug was row shape and discarded attempt history. `bedrock_converse.go` was fixed for
free, since it already routes through `recordBedrockPartialFailure`.

Status is reported as **200 deliberately**: headers were written and flushed with 200
before the break, so 200 is what the client received. Deriving 502 would describe a
response nobody was sent.

**Design mistake worth knowing about.** My first attempt copied 18f's shape — pair
`emitTrace` with a new *non-counting* row helper. Wrong: `emitTrace` already appends the
row **and** counts, so that wrote **two rows per failure**, re-creating the very D1 defect
being extended. The remedy was a **deletion**, not a split. 18f's collision was two
*counting* helpers; 18g's was one helper already doing both jobs. Read the emitter before
pairing anything with it.

**THE LESSON, REPEATED — and this is the part to actually change behaviour on.** The
"mutate the call site" rule from 18f was already written in this file *and* in the skill. I
still wrote five tests that drove the new choke point directly, and the battery said:

```
[SURVIVED] M1 bedrock CALL SITE reverted to legacy row
[SURVIVED] M2 custom_api CALL SITE reverted to legacy row
```

Reverting either call site — undoing the entire fix — passed all 1379 tests. Two
compounding causes, both worth checking by hand next time:

- the fix is **side-effect-neutral by design** (one counter bump either way), so no
  counter assertion can separate fixed from broken — the discriminator is row *shape*
  (`RequestID != ""`, `AttemptCount`);
- an existing test *did* touch the call site and proved nothing:
  `bedrock_partial_failure_test.go` builds `forwardParams` with **no** recorder, so it
  takes the untraced fallback. Covering a line is not covering the branch the fix is in.

Killed by two tests driving the real callers:
`TestBedrockPartialFailureCallSiteEmitsRichRow` and
`TestCustomApiPartialFailureCallSiteEmitsRichRow` (the latter runs the real
`streamUpstream` loop against a body that yields one SSE chunk then errors, and asserts
the client actually received bytes so it cannot pass vacuously).

**Run it as a procedure, not a maxim:** write the test → mechanically revert the *call
site* → confirm the suite goes red → restore. Recorded in the skill in that form.

**Verified:** 7 tests, **8/8 mutants killed** by distinct tests, post-battery diff shows
only the intended edits; full suite **1381 passed**; full-repo `-race` **1381 passed in 6
packages** at HEAD `4be1377`. Defect count **81 → 82**. Remaining trace gap: **D1b**
(websearch pair, multi-account attribution).

**Stale async results nearly got reported as this round's evidence — twice.** Two
background `go test ./... -race` jobs launched in earlier rounds both finished *after* 18g
was committed, returning `1369 passed` (the 18e baseline) and `1374 passed` (the 18f
baseline). Each ran against a working tree that no longer exists, and both numbers are
close enough to the real 1381 to survive a glance.

Re-run at the current HEAD before quoting any number a background job hands back, and print
`git log --oneline -1` in the same command so the output carries the commit it measured. An
async figure with no commit attached is not evidence — and expect more of these, since any
long-running job in a moving repo produces them.

### Round 18h — D1b: the websearch pair, and the LAST trace gap

`proxy/websearch.go`, `proxy/websearch_loop.go`. Full reasoning in the checkpoint §13.

The final direct callers of the legacy row writers. `websearch.go:700` and
`websearch_loop.go:223` called `recordSuccessLog`, plus four failure sites on
`recordFailureWithDetails`, so both web-search surfaces produced thin rows with no
`RequestID` and no `Attempts`. The loop was the worst case in the repo: one request spans
up to `maxWebSearchRounds` upstream rounds and the pool's LRU deliberately sends
consecutive rounds to **different** accounts, so a single request routinely touched several
accounts and none of that history was recorded anywhere.

**Shape (user chose):** ONE row per request, every round's attempts accumulated on one
recorder. Request-level `AccountID` falls out of `emitTrace`'s existing last-attempt rule,
which here is the terminal round's account; `Attempts[]` carries every account tried.

**Two structural facts that made this NOT a copy of D1/D1d** — assume either away and the
fix is broken:

1. **The recorder is CREATED, not threaded.** Both entrypoints dispatch from
   `handler.go:1902-1913`, *before* that function's first `newTraceRecorder` (`:1953`).
   There is nothing upstream to inherit.
2. **Attempts open INSIDE the callee loops.** `performWebSearch` (`websearch.go:482`) and
   `callUpstreamForWebSearch` (`websearch_loop.go:273`) select their own account and return
   only the one that served, so a caller-opened attempt records ~0ms and loses every
   account that failed first. In `callUpstreamForWebSearch` the attempt opens *after* the
   Bedrock/custom_api eligibility skip — those accounts are ineligible, not broken, and no
   dispatch happens, so an attempt there would invent a failure.

**The skill said SIX retry loops. There are EIGHT** (`handler.go` 2049/3047/3420/3901,
`responses_handler.go` 180/529, `websearch.go` 482, `websearch_loop.go` 273) — and the two
it omitted are exactly the two this round had to instrument, so the stale count pointed
*away* from the work. Corrected in the skill with the `search_files` command to re-derive
it instead of a number to trust.

**My implementation was wrong and a test caught it pre-commit.** First version added the
`tr` parameter to `callUpstreamForWebSearch` and a doc comment describing per-round
attempts, but never added the `beginAttempt`/`endAttempt` calls inside the loop —
signature threaded, behaviour absent, doc comment a lie the compiler cannot catch.
`TestWebSearchLoopEmitsOneRichRowAcrossRounds` failed with `AttemptCount = 1 but the
request made 2 upstream rounds` (the lone attempt came from the MCP search between rounds).
That assertion exists because of the §12d procedure: assert the row shape a caller
observes, not that a helper was called.

**Then the battery caught the tests.** First run killed 8/10; **M9** (`beginAttempt(nil)`
— attempts lose account identity) and **M10** (`endAttempt(att, nil)` on the failure branch
— a failed round recorded as success) both SURVIVED, because every loop assertion I had
written checked attempt *count*, and both mutants leave the count correct. Closed with
`TestWebSearchLoopRecordsFailedRoundAttemptWithItsCause`, which drives a real failover
(first upstream call 500, retry succeeds) and asserts per-attempt `AccountID` and
`Outcome`/`Error` rather than the count.

**Lesson, third variant of the same class:** count assertions do not constrain content.
18f/18g were "mutate the call site"; this one is "mutate the *field*" — for anything that
accumulates records, assert identity and outcome per record, not just how many there are.

### 2026-07-30 production recovery — external termination, then round 17 deploy

The old production container was found stopped with exit code 2 about 20 seconds
after startup. The available container log ended at the startup banner and had no
panic or fatal line. Do not rewrite this as an application crash: the old image was
run twice with an anonymous data volume containing a byte-identical copy of live
`config.json`, first minimally and then with the production watcher + AWS-cache
environment. Both runs stayed healthy beyond the original failure window.

The discriminating experiment was signal injection. Sending either SIGINT or
SIGTERM to the old image produced the exact observed signature: exit code 2, no
stack trace, and no terminal log line. At deployed commit `29e799c`, the only
code-initiated terminal path was `logger.Fatalf` -> `os.Exit(1)`; there was no
signal handling or self-signal call. The original container healthcheck had also
returned 200 five seconds after startup. Therefore the evidence classifies the
event as an **external termination signal**, not a startup/runtime panic. Docker
event history for the window was no longer available, so the identity of the
sender is unknown and must remain unknown.

Before deployment, `go build ./...`, `go vet ./...`, tree-wide `gofmt -l`, and
`go test ./... -race -count=1` all passed. The new image was smoke-tested with a
config copy for 25 seconds, then SIGTERM-drained with exit 0 and the expected four
shutdown log stages. Production was recreated with `--no-build`; its bind-mounted
live config retained SHA-256
`09a5963de101ad7a5273fa356235a4bddc375c3b97375073b679a51aed1e7e56` across the
swap. No inference request was generated merely to prove deployment.

### Round 16 — the 402/overage path never parked the account (defect 78)

`handleAccountFailure`'s overage branch called `disableAccountOverage` +
`RecordError(account.ID, false)`. Neither parks the account: the former only
re-reads the upstream Overages switch (and returns early if that live fetch
fails), the latter files a 402 as a GENERIC failure needing 3 consecutive errors
before even a 1-minute cooldown. Meanwhile `pool.MarkOverLimit` — which applies
the correct 1h backoff via `setCooldownIfLater` — had ZERO non-test callers, even
though the repo's own design spec says `402 -> pool.MarkOverLimit`.

Measured on the live corpus: of 21 tagged 402-overage events on one account,
**20 re-selected that same account within 60s, median gap 0s**.

Fix: `h.pool.MarkOverLimit(account.ID)` first, BEFORE the status refresh (ordering
is load-bearing — fetch-first means a slow/failing fetch delays or skips the
backoff). Commit: see `git log --oneline -3`. Test: `proxy/overage_backoff_test.go`
(6 tests; neutralization fails exactly the 2 behavioural ones, 4 controls green).

STILL OPEN from this: the backoff is a flat 1h, not "until `NextResetDate`". For a
MONTHLY cap that is still far too short. Sizing from `NextResetDate` is the
remaining half — and needs care, since a wrong reset date parks a healthy account
for weeks.

---

## 3. What the user cares about — learned, not guessed

- **Never fabricate completeness.** Report blockers honestly instead of inventing
  output. Self-correct a false "done" immediately and plainly.
- **Green tests prove nothing until RED-proven.** For every fix: write the test,
  watch it FAIL against the unfixed code, then fix. For high-value fixes,
  neutralize the fix afterwards and confirm the test goes red again. A test that
  passes both with and without the fix is a **false green** — delete it, do not
  keep it.
- **Re-derive subagent claims on live bytes.** Reviewer reports are leads, not
  facts. Several were wrong; several probes had *inverted* assertions that
  "confirmed" defects that did not exist.
- **Adversarial review is expected.** Dispatch subagents on non-trivial changes,
  especially your own. 15 of the 67 defects were regressions introduced by
  earlier fixes in the same series, each caught by re-reviewing the diff — never
  by trusting the original reasoning.
- **Docs are part of the change set.** Update the checkpoint in the same pass as
  the code. Record corrections rather than silently patching them.
- **Confirm pushes against the remote** (re-fetch, compare SHAs) — not just the
  push output.
- **Terminal demos:** ASCII only. Accessible prose. No rambling.

---

## 4. Mandatory verification gate

Run all of this before claiming anything is done:

```bash
cd /home/harry-riddle/dev/github.com/0xharryriddle/Kiro-Go
gofmt -l ./config ./proxy ./pool ./auth   # expect EMPTY
go build ./...
go vet ./...
go test ./config/ ./pool/ ./auth/ ./proxy/ -count=1
go test -race ./config/ ./pool/ ./auth/ ./proxy/ -count=1
node --check web/app.js
git diff --check
```

`gofmt` is clean tree-wide right now, so any output means **your** change caused it.

---

## 5. Deploy procedure — and the traps in it

```bash
docker compose build                       # build WITHOUT swapping
# verify the image before deploying (see below)
docker compose up -d                       # run via background=true; the guard
                                           # flags it as long-lived otherwise
curl -s http://127.0.0.1:8080/health       # expect version 1.1.5
```

**Trap 1 — committing and pushing deploys nothing.** The Dockerfile compiles from
source at build time. The container ran pre-merge code for ~37 hours before anyone
noticed. Always rebuild and verify.

**Trap 2 — verify the image by TAG, never by digest.**
`docker compose images -q` returned a stale digest the daemon no longer had, so
`docker save` silently produced nothing and every symbol read as ABSENT. Extract
like this:

```bash
cid=$(docker create kiro-go-kiro-go:latest)
docker cp "$cid:/app/kiro-go" /tmp/check_kiro-go
docker rm "$cid" >/dev/null
```

**Trap 3 — always use a positive control on symbol checks.** Grep for a symbol you
KNOW exists (`listKiroProfilesInRegion`) alongside the new one. Without it you
cannot distinguish "missing code" from "broken probe".

**Trap 4 — Go `const` emits no symbol.** Inlined constants (`maxMcpResponseBytes`,
`kiroProfilePageSize`, `modelsRefreshMinInterval`) will read ABSENT even when
present. Grep for their **values** or their effects instead.

**Rollback:** the immediately-pre-deploy image is deliberately retained as
`kiro-go-kiro-go:rollback-71f4e867` (full digest and extracted-binary SHA are in
section 2). This tag was created **before** `docker compose build`, so moving
`:latest` did not destroy it. Verify its image ID before using it; do not rebuild an
old checkout when the exact deployed artifact is already available.

---

## 6. Open work — highest risk first

### 6a. Unreviewed files with no test coverage

Line counts below are measured, not remembered (`wc -l`).

| File | Lines | State | Why it matters |
|---|---|---|---|
| `proxy/admin_bot_api.go` | 1441 | **UNREVIEWED** (bodies bounded in `16a71ad`, no sibling test) | largest un-audited file in the repo; admin surface — the strongest remaining target |
| `proxy/bedrock_converse.go` | 919 | audited rounds 11 + 13 → defects #70, #72, #73 | Bedrock Converse translation; partial-stream AND client-disconnect accounting now pinned |
| `proxy/bedrock_openai.go` | 809 | streaming/accounting path audited round 13 → defects #72, #73 | **request TRANSLATION half still unreviewed**: `openAIToAnthropicMessages` and its helpers (system/tool/image conversion) have no adversarial pass |
| `proxy/custom_api_forward.go` | 634 | read in round 13 as the reference contract (its client-gone handling is what rounds 72/73 mirrored); **no defect pass yet** | transparent passthrough; trust boundary |
| `proxy/bedrock.go` | 637 | streaming/accounting path audited round 13 → defects #72, #73 | prompt-cache survival through `buildBedrockBody` still unverified |
| `proxy/request_trace_recorder.go` | 483 | **UNREVIEWED** | decides what reaches disk; PII/redaction relevance. See R13-followup: Bedrock paths never reach it at all |
| `proxy/websearch_loop.go` | 691 | audited round 9 → defect #67 | the whole loop was read; accounting now pinned |
| `auth/sso_token.go` | 378 | audited round 9 → defect #66 | credential handling; redaction test added |

Method that worked: find the largest non-test file lacking a sibling `_test.go`,
read it, look for (a) unbounded reads/allocations, (b) inconsistency with a
sibling that already does it right, (c) invariants nothing pins, (d) values
accumulated across iterations of a loop where only the last one survives.

**One caveat learned in round 9:** `proxy/websearch_loop.go` had no
`websearch_loop_test.go` but FIVE other test files exercised its symbols, so
"no sibling test" overstates how uncovered a file is. Run
`grep -l '<symbol>' proxy/*_test.go` before assuming zero coverage.

**Second caveat:** the round-9 audit began from a stale task list that named
three files as unreviewed; two had already been handled, and one line-count loop
silently reported `0 lines` for all three because the paths were wrong. Re-measure
before trusting any inventory in this document.

### 6b. Failure frames — RESOLVED in round 18 (this section changed; read it)

**This item is no longer blocked, and the policy it described is no longer what the
code does.** It used to say: `parseEventStream` observes AWS exception frames
(`:message-type: exception|error`), logs them, and never treats them as fatal —
promoting any `:exception-type` needed a real observed frame first.

The second merge (`hian699` v1.2.8) forced the question, because upstream had
implemented the opposite policy and its test contradicted the fork's four. The
resolution is **drain-then-error**, and it is neither side verbatim:

- the failure frame is recorded, the loop **keeps draining**, and every subsequent
  content frame is still delivered to the client;
- the error is returned only at **end-of-stream** (`failureFrameErr`, `proxy/kiro.go`),
  so it costs no client text;
- `OnComplete` still fires, so usage a failure frame carried is still billed.

The old "do not force it" reasoning was about *aborting mid-answer*. Draining removes
that risk entirely, so the blocker did not apply to this shape. What did apply is the
opposite risk, which returning `nil` had left open: a false success clears the
account's cooldown via `pool.RecordSuccess` and bills the customer key an estimated
input total — the same failure class round 12 closed in this same function.

Still true, and still the reason nothing is promoted per-type: **no real Kiro
exception frame has ever been observed.** The corpus cannot supply one
(`noteResponseText` stores assembled text, so headers are destroyed before capture).
Only throttling is mapped to a status (`HTTP 429`, so `isQuotaErrorMessage` matches
it); everything else stays a generic upstream failure, because the 403 path
*disables* an account and a mis-inference there costs a working account.

Evidence counter, still worth checking as frames accumulate:

```bash
docker logs kiro-go-kiro-go-1 2>&1 | grep -c "upstream failure frame"
```

Zero when last checked — and note the container was **not running** at that point, so
that zero is not evidence of absence. See §2.

**Do not "restore" the observation-only policy** without reading the round-18 entry
in the checkpoint: five test assertions encode this decision, each carrying a
`POLICY CHANGE` comment explaining what replaced it.

### 6c. Closed by operator decision — do NOT "fix"

**The proxy is intentionally open.** `requireApiKey = false` with zero API keys, so
every customer route including `POST /v1/messages` returns 200 without a
credential. This is **deliberate**: internal proxy, trust boundary is the network.
The user's words: *"No need because this is internal proxy, only the /v1/messages
need the api if it's required."*

**Never flip `requireApiKey` to true as hardening.** With zero keys defined it 401s
every request — an outage. Safe order only: mint a key → update clients → enable.

### 6d. Settled — do not re-investigate

- **Prompt caching cannot work on the Kiro path.** Proven empirically, not
  deduced: 38 parsed real payloads and 1,200 scanned bodies show no cache field at
  any level. `KiroUserInputMessage.Content` is a plain `string` — nowhere for
  per-block `cache_control`. Live tracker: 0 hits / 0 misses over 33k+ requests.
  `cacheRead: 0` in the logs is the true answer, now explicit rather than absent.
  Bedrock is where caching works (`cache_control` survives `buildBedrockBody`).
- **`toolResult.status`** — the corpus only ever shows `"success"` because we
  hardcode it. Reopening needs upstream DOCS, not another capture.
- **Scan contamination warning:** a naive text search for cache markers reported
  "262 of 600 bodies contain cache markers". All false — the captures contain the
  agent session that was *investigating* caching, so the search matched its own
  shell commands. Require a JSON-key match and exclude your own tooling.

---

## 7. Known-good facts worth not rediscovering

- **`kiroProfilePageSize = 10` is a hard upstream limit.** Boundary-swept live:
  1/5/10 pass; 11 and above return HTTP 400 `REQUEST_BODY_INVALID`. The merge had
  hardcoded 50, which broke profile resolution for every account without a cached
  `profileArn` and made paid plans display as "Free".
- **Two auth classifiers must agree.** `proxy.isAuthErrorMessage` and
  `pool.IsAuthFailure` are pinned by a cross-package agreement test over 18 real
  formatter strings. Rule: a 5xx must never let body markers trigger a ban, and a
  number only counts as a status when introduced as one (`http`/`status`/
  `returned`/`failed` before it) — otherwise `usage 512/1000` reads as a 5xx.
- **Redaction must not destroy diagnostics.** `isAuthErrorMessage` classifies
  revoked credentials by finding `invalid_grant`/`invalid_token`. The redactor
  must never redact a value that IS a known OAuth error code, and a bare `:` is
  prose, not an assignment (only `=`, or `:` with a JSON-quoted key).
- **Anthropic error enum, verified live:** 400 `invalid_request_error`, 401
  `authentication_error`, **402 `billing_error`**, 403 `permission_error`, 404
  `not_found_error`, 413 `request_too_large`, 429 `rate_limit_error`, 500
  `api_error`, 504 `timeout_error`, **529 `overloaded_error`** (not 503).

---

## 8. Subagent handling

- **Dispatch pinned reviewers for anything non-trivial**, including your own fixes.
- **A created delegation directory is NOT evidence of a live worker.** One batch
  died silently from a `database is locked` error while its directories existed;
  logs sat frozen at 1807 bytes for 29 minutes. Verify **log growth**:

```bash
for f in ~/.hermes/cache/delegation/live/<deleg_id>/task-*.log; do
  stat -c '%n %s %y' "$f"
done   # run twice, a minute apart — size must increase
```

- Reviewer probes are often left behind. Remove only the **exact** `zz_*` paths
  you or they created, and only after the worker has finished. A reviewer probe
  once got swept into a merge commit by `git add -A`.
- Probes with unconditional `t.Fatalf` bodies are worthless — rewrite findings as
  real assertions before keeping them.

---

## 9. Tooling quirks that will waste your time

- The terminal guard **blocks** `$(...)` command substitution, `{{...}}` Docker
  format braces, `find -exec {}`, and `...` inside commands. Use the dedicated
  `search_files` / `read_file` tools, or plain sequential commands.
- `rg` is shadowed — use `search_files`.
- `go test` output is rewritten into a summary line by a wrapper. For raw output,
  `go test -c -o /tmp/x.test ./proxy/ && /tmp/x.test -test.run X -test.v`.
- `web_search` / `web_extract` return **HTTP 402 (out of credit)**. Plain `curl`
  works — fetch specific doc URLs directly.
- `write_file` overwrites whole files and caps around 350 lines; use `patch` for
  edits to existing files, with literal quotes (never backslash-escaped).

---

## 10. Why this is a fresh session

Context compaction was stuck in the previous one: 834k tokens against a 500k
threshold with `compression_count: 0` and an empty `noop_reason`. Cause was a
stuck runtime frontier — `current_frontier_store_id: 0` while every message sat
above store_id 407,889, so the engine found nothing compactable and idled. A new
session binds a new frontier, so compaction should work normally here.

Compression is already configured to a nous model
(`auxiliary.compression`: `provider: nous`, `model: qwen/qwen3.5-flash-02-23`) —
that needs no change. **Do not apply the plugin's preset suggestion**: it is marked
`read_only`, its model-family match was never verified, and it would RAISE the
threshold 0.5 → 0.75, firing later rather than sooner.

The old conversation remains searchable via `lcm_grep` and `session_search`.
