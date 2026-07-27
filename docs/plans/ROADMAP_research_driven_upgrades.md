# Kiro-Go — research-driven upgrade roadmap (round 14 research pass)

Date: 2026-07-28. Baseline commit: `ec6784d` (clean tree, 947 tests green pre-round-14).
Live deployment at research time: image `71f4e867`, version 1.1.5, healthy.

## 0. How to read this document

Every claim carries an evidence class. This is the same discipline the audit
checkpoint uses, and it matters more here than usual because a roadmap is where
unverified assumptions do the most damage:

- **MEASURED** — computed from this deployment's own trace corpus
  (`data/traces/index-2026072{5,6,7}.jsonl`, 17,784 indexed requests, 17,676
  captured bodies) or from a benchmark run on this host. Numbers are reproducible.
- **CODE-VERIFIED** — read directly in the repo at `ec6784d`, with file:line.
- **EXTERNAL** — from public documentation of other projects / standards, via
  research subagents. Not verified against this codebase.
- **REFUTED** — a hypothesis I held during this pass that the evidence killed.
  Recorded so it is not re-proposed.

## 1. What the workload actually is (MEASURED)

This shapes every priority below, and it is not what a generic "LLM gateway"
roadmap would assume.

| Property | Value | Source |
|---|---|---|
| Indexed requests (3 days) | 17,784 | trace index |
| Modern-format rows | 17,343 | trace index |
| Peak throughput | **62 req/min = 1.03 req/s** | busiest minute |
| p99 minute | 39 req/min | trace index |
| Mean active rate | 11.8 req/min over 1,502 active minutes | trace index |
| Endpoint mix | 17,775 openai / 9 claude | trace index |
| Model concentration | 17,558 of 17,343+ rows on `claude-opus-5` | trace index |
| **Tool-call turns** | **16,384 / 17,343 = 94.5%** | `stopReason=tool_calls` |
| Billed input tokens | 3,243,544,206 | trace index |
| Billed output tokens | 15,444,953 | trace index |
| **input:output ratio** | **p50 = 245:1, mean = 742:1** | per-row |
| Credits recorded | 24,031.8 | trace index |
| TTFB | p50 5,169 ms / p95 14,115 ms / p99 25,448 ms | success rows |
| Failure rate | 194 / 17,343 = 1.1% | trace index |
| Distinct accounts serving | 14 | trace index |

**The single most important fact: this is an agentic tool-calling workload, not a
chat workload.** 94.5% of requests end in `tool_calls`, and the agent resends the
whole conversation each step, which is why input dwarfs output 245:1.

Prefix repetition, measured across 2,788 consecutive request pairs from 2,991
parsable captured bodies in 203 conversations:

- mean history turns resent per follow-up request: **27.1**
- mean turns that are an EXACT repeat of the previous request: **24.6**
- **repeated-prefix share: 90.6% by turns, 95.2% by bytes**
- absolute: 367,416,972 of 385,812,454 resent bytes are exact repeats

## 2. REFUTED hypotheses (do not re-propose without new evidence)

### R1. "Prompt caching would cut ~85% of credit spend." REFUTED.

I derived this from the 95.2% repeated-prefix share and it looked like the
headline finding of the whole pass. It does not survive its own test.

Credits are **not token-proportional**. Ground truth in code: the Kiro path takes
credits from the upstream `meteringEvent` (`proxy/kiro.go:936-939`, surfaced via
`OnCredits` at `:953-955`) — the proxy never derives them from tokens. Measured on
17,140 rows carrying both:

- `r(inputTokens, credits) = 0.5945` — weak for a supposedly linear relationship
- CoV(credits per request) = **0.573**
- CoV(credits per 1k input tokens) = **0.988**

Credits per request vary *half* as much as credits per token, i.e. billing sits
closer to per-request than per-token. A cache that removes 95% of resent input
bytes therefore does **not** remove 95% of spend. Any savings claim needs a
controlled experiment against `meteringEvent`, not a token model.

What prompt caching would still plausibly buy is **latency** (p50 TTFB 5.2 s on
p50 245:1 input:output is dominated by prefill). That is worth having, but it is a
latency project, not a cost project, and it must be sold as such.

### R2. "The per-request `config.Save()` fsync is the scaling bottleneck." REFUTED at current volume.

CODE-VERIFIED: every billed request calls `config.RecordApiKeyUsage`
(`proxy/handler.go:2271`) → `saveLocked()` (`config/apikeys.go:248`) under the
global `cfgLock`, and `atomicWriteConfig` (`config/config.go:835-867`) does
read + backup-rotate + write + `fsync` + `rename` of the whole file. `UpdateStats`
adds a second save on a detached goroutine (`pool/account.go:1286+`). Live
`config.json` is 142 KB with 6 rotated backups, so a billed request amplifies to
roughly 875 KB of writes.

MEASURED on this host: 2.04 ms per serial atomic write, 2.18 ms under 16-goroutine
contention → ceiling ≈ **459 req/s** with one sync save per request, **230 req/s**
counting the detached one.

Observed peak is 1.03 req/s = **0.45% of that ceiling**. It does not bind. Revisit
only if sustained rate approaches ~50 req/s, or if `config.json` grows an order of
magnitude. Recorded because it is the kind of finding that reads as urgent and is not.

### R3. "The 402 monthly-cap error is misclassified, so failover never fires." REFUTED as a live defect.

The corpus shows 138 `MONTHLY_REQUEST_COUNT` events, 112 of which reached the
client as HTTP 500 / `errorType: unknown` with `attemptCount=1`. That looks exactly
like a broken classifier, and `isOverageErrorMessage` (`proxy/account_failover.go:201-206`)
does require BOTH a `402` token AND the literal word `overage` — which
`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}` lacks.

But `upstreamError` (`proxy/kiro.go:425-430`) *injects* the word: 402 becomes
`HTTP 402 overage from %s`. Splitting the corpus by form:

- 117 untagged (`HTTP 402 from Kiro IDE`), all 07-25 12:08–15:17
- 21 tagged (`HTTP 402 overage from`), all 07-27 14:10–14:38
- exactly **1 transition** between forms → a clean deploy cutover, not two live paths

`git log -L` on the function confirms the tag landed in `d680e8c` and was present
from `99dda52` (07-25) onward. The untagged rows are pre-merge history. The live
path classifies correctly.

## 3. CONFIRMED findings from this pass

### F-A. `/v1/responses` had no `IsBedrock()` guard — FIXED this pass (round 14)

CODE-VERIFIED and now closed. Both `/v1/responses` dispatch loops selected accounts
via `GetNextForModelWithApiKey` and branched on `IsCustomApi()` but **not**
`IsBedrock()`, then fell through to `CallKiroAPIWithDiagnostics`
(`proxy/responses_handler.go:233` and `:587`). The Claude and OpenAI surfaces both
branch on `IsBedrock()`; this third surface did not.

Reachability was checked rather than assumed: the pool has no Bedrock filter,
`accountHasModel` fails open on a cold cache (`pool/account.go:436`), and
`ensureValidToken` early-returns nil for Bedrock (`proxy/handler.go:3625`). So a
Bedrock account was selectable and would be dispatched with Kiro OAuth semantics,
403, and then be penalised — potentially banned — by `handleAccountFailure`.

**Latent, not live:** this deployment currently has zero Bedrock accounts
(24 accounts: 18 `idc`, 3 `external_idp`, 3 `api_key`), so it cannot fire today.
It becomes live the moment a Bedrock account is added. Directly violates CLAUDE.md:
"If you add a new Kiro-facing loop, add an `IsBedrock()` guard."

Fix: skip (not dispatch) Bedrock accounts on both loops, `excluded` + `attempt--`,
matching the sibling `IsCustomApi()` contract. There is no OpenAI→Anthropic
Responses translation yet, so skipping is the correct behaviour, not translating.

**A false-green worth recording.** My first test asserted on *source text*
(`strings.Contains(src, "IsBedrock()")`). Neutralizing the guards to
`if false && account.IsBedrock()` left the substring present, so both tests still
passed — a test that cannot fail. Replaced with a behavioural test that stands up a
Bedrock-only pool plus a fake Kiro endpoint and asserts the endpoint is **never
hit**; under neutralization it now fails with "a Bedrock account was dispatched to
the KIRO endpoint 1 time(s)" while the Kiro-dispatch control stays green.

### F-B. Quota state is up to 30 minutes stale, with no in-flight accounting (CODE-VERIFIED + MEASURED)

`Account.UsageCurrent` is written **only** by the 30-minute `backgroundRefresh`
ticker (`proxy/handler.go:514`, persisted at `config/config.go:2043`). Confirmed by
exhaustive search: there is no `UsageCurrent++`, no `+=`, no `IncrementUsage`, no
`reserveQuota`, no `inFlight` anywhere in the tree (all zero matches).

Consequence, MEASURED on `thuquan-pham@…`: 1,933 successful requests served before
its first cap, **302 of them inside the final 30-minute window**, then 112 cap
errors in 8 minutes with only 3 successes interleaved. The cap is terminal, not
intermittent — the cached view said under-limit while the real counter was spent.

Scale check: at the observed 62 req/min peak, one refresh window admits **1,860
requests**. The smallest configured cap in this fleet is 1,000
(`minhdung-truong@…` at 1000/1000), so a single stale window can overshoot a small
account's entire monthly cap by 1.9x.

Note the dimension: `UsageLimit` values here are 5000/10000 — these are
**AGENTIC_REQUEST counts**, not credits (`config.go:187` comments call them
credits, which is misleading). `isOverUsageLimit` (`pool/account.go:1498`) compares
the right numbers; the comment is wrong.

### F-C. The overage path applies no cooldown (CODE-VERIFIED + MEASURED)

`handleAccountFailure`'s overage branch calls `disableAccountOverage` +
`RecordError(false)` (`proxy/account_failover.go:453-455`). `disableAccountOverage`
only refreshes and persists the upstream `OverageStatus` snapshot (`:414-431`) — it
sets **no cooldown**.

`MarkOverLimit` (`pool/account.go:1188`), which *does* set a 1-hour cooldown, has
**zero non-test callers**. It is dead code.

MEASURED consequence on the 21 correctly-tagged cap events: the same account was
re-dispatched after a cap in **7 s min / 77 s median / 274 s max**; 20/20 inside an
hour, 6/20 within a minute. A hard-capped account stays in rotation and keeps
burning round-trips (median wasted upstream latency 1,716 ms per cap event).

### F-D. Latency-aware routing has a real, controlled opportunity (MEASURED)

Raw per-account TTFB p50 spread is 4,399–7,582 ms (1.72x), but that is confounded:
`r(account median input tokens, account TTFB p50) = 0.603`, and normalizing by
input tokens *inverts* the order (the "slowest" account has the smallest prompts),
so fixed overhead dominates.

Controlling properly, by comparing accounts **within** input-size bands:

| band | accounts | within-band TTFB p50 spread |
|---|---|---|
| 0–25k | 4 | 1.04x (4,545 → 4,716 ms) |
| 25–50k | 5 | 1.18x (4,245 → 5,008 ms) |
| 50–100k | 7 | 1.42x (4,230 → 6,024 ms) |
| 100–200k | 9 | 1.47x (4,782 → 7,036 ms) |
| 200–400k | 8 | 1.46x (4,823 → 7,026 ms) |

Median within-band spread **1.42x** — a genuine per-account speed difference, not a
workload artifact. And there is a choice to make: **68.1%** of 5-minute buckets had
more than one account active.

CODE-VERIFIED: `healthScore` already blends an EWMA latency factor
(`pool/account.go:1634-1643`) and is ungated, but it only breaks ties *among
equally-old LRU candidates*, so once all accounts have been used LRU dominates.
Quota-aware mode (`GetQuotaAwareRouting`) sorts purely by remaining quota with no
latency or concurrency term — **and it is OFF in the live config**.

### F-E. Bedrock paths never emit a structured trace row (CODE-VERIFIED, pre-existing)

Carried from round 13 and still open. Both Bedrock dispatch branches call
`tr.beginAttempt(account)` then `return` without `tr.endAttempt` or `h.emitTrace`
(`proxy/handler.go:1737-1752`, `:2872-2885`); Bedrock accounting uses the legacy
`recordSuccessLog` helper (`proxy/bedrock.go:528`) instead. Bedrock traffic
therefore has no `Attempts[]`, no `TTFBMs`, no `RequestID` correlation.
Observability-only — billing and health go through the legacy path — but it is also
why the round-12 corpus reasoning was weak for Bedrock.

## 4. EXTERNAL landscape (research subagents, not verified here)

Four researchers ran against public sources; three produced usable output.
Summaries retained at
`/home/harry-riddle/.hermes/cache/delegation/subagent-summary-{0,1,2}-2026072*.txt`.

**Read this section knowing how the research was actually obtained.** The managed
web-search/extract backend (Parallel) was returning HTTP 402 "Insufficient credit"
for the whole pass — reproduced directly from the parent client, not merely
reported by a child. Every researcher therefore fell back to `curl`/`urllib`
against raw GitHub READMEs, the GitHub API, release feeds, and official docs.
That is a real limitation on *discovery*: projects nobody named up front were never
surfaced, so absence from the list below is not evidence of absence in the market.
Claims that were read directly from a project's own README or docs are sound.

**One researcher's central caveat is false and is not repeated here.** The
landscape researcher concluded "the promised target inventory was not actually
present" and consequently marked every target-side comparison UNVERIFIED. Checking
its own transcript: the inventory *was* delivered (4,663 characters of it, in the
`context` field — the live log truncates for display only). It had ground truth and
declined to use it. Its competitor-side data is retained below; its target-side
"gaps" were re-derived here against the actual code rather than accepted.

A second researcher asked for a repository URL and returned no research at all.
Subagents cannot call `clarify`, so a question is a dead end — the task simply
burned. Worth remembering when dispatching: state the target inline or expect this.

The market has split into three families: enterprise/API-key gateways (LiteLLM,
Bifrost, Portkey, Envoy AI Gateway, Higress, APISIX), distribution/billing
platforms (New API, One API), and **OAuth/subscription account pools** (Sub2API,
CLIProxyAPI, Claude Relay Service, AIClient2API, OmniRoute).

Kiro-Go is in family 3. What that family treats as **table stakes** and Kiro-Go
already has: OpenAI + Anthropic compatibility, multi-account pooling, OAuth
onboarding with refresh, rotation, cooldowns, circuit breaking, downstream keys
with quotas, admin UI, Docker, request logging. Kiro-Go is genuinely ahead of most
of family 3 on failure classification, attempt-level tracing, and audit rigour.

What the researchers flag as **current differentiators** Kiro-Go lacks, and my
assessment of each against the measured workload:

| External claim | Verdict here |
|---|---|
| Per-account concurrency reservation / in-flight accounting | **Matches F-B.** Real gap, real evidence. |
| Quota-window-aware scheduling (5h/daily/provider reset) | **Matches F-B/F-C.** Real gap. |
| Sticky sessions to preserve prompt-cache affinity | Kiro-Go HAS API-key affinity (`apiKeyBinding`). Prefix-cache-aware routing is the missing half — relevant given 95.2% prefix repetition. |
| `context.Context` propagation + graceful shutdown | CODE-VERIFIED gap: `CallKiroAPIWithDiagnostics` takes no ctx; `main.go` calls `ListenAndServe` directly. Genuine, and cheap. |
| Typed upstream errors instead of `err.Error()` parsing | CODE-VERIFIED. The classifiers are hardened but string-based; R3 above is exactly the fragility this predicts. |
| Real OTel export + GenAI metric conventions | CODE-VERIFIED gap: the trace model is *semantically* OTel-shaped (deliberately, per `handler.go:75`) but `go.mod` has only `uuid`. Conflicts with the zero-dependency invariant — see §6. |
| Redis/shared state for multi-replica | Not a gap **for this deployment**: single node at 0.45% of its write ceiling. Do not build. |
| Embeddings / batches / audio / files endpoints | Absent (CODE-VERIFIED). Zero demand in corpus: 100% of traffic is chat/responses on one model family. Low priority. |
| Unknown request fields silently ignored | CODE-VERIFIED (`json.Unmarshal` into narrow structs, no `DisallowUnknownFields`). Real correctness risk: a client believes `tool_choice`/`stop`/`response_format` were honoured when they were dropped. |

### 4b. Candidate gaps from the second research batch — TESTED, mostly false

A second batch of researchers named further differentiators. Rather than append them
as "gaps", each was checked against the code. The check changed the answer often
enough to be worth recording:

| Candidate | Verdict after checking the code |
|---|---|
| Hashed key storage | **ALREADY SHIPPED — not a gap.** `config/apikeys.go:5,24` hashes customer keys with sha256, and carries a comment explaining why sha256 rather than bcrypt/argon2 (keys are high-entropy random, so a slow KDF buys nothing). `config.go` additionally hashes refresh tokens and Kiro API keys. My first search for this returned *zero* hits and I nearly recorded it as missing — the pattern was case-wrong, not the feature absent. |
| SSRF protection on operator-supplied upstream URLs | **PARTIAL, and low value here.** `normalizeBaseURL` (`proxy/custom_api_forward.go:288-312`) requires an http(s) scheme, a non-empty host, and rejects query/fragment; it is wired into the admin add path (`admin_bot_api.go:1348`). What it does *not* do is block loopback/link-local/private ranges. But this input is operator-supplied behind admin auth, not customer-supplied — so it is hardening, not a live hole. |
| Least-busy routing, capability-aware fallback, route explainability, dry-run route simulation, MCP gateway, OIDC/RBAC admin, envelope encryption, distributed refresh lock | Genuinely absent. Of these, only **route explainability** is cheap and useful at this scale (the trace model already records per-attempt selection data; it just never records *why* an account was chosen). The distributed refresh lock is meaningless single-node. MCP gateway, OIDC/RBAC, and envelope encryption are product decisions, not defects. |

**Method note, because it caught me out.** Nine of these were flagged as "gaps" by a
keyword scan of my own roadmap versus the reports. I then checked two of them against
the code, and *both* came back shipped or partly shipped — a 2-for-2 false-positive
rate on the only ones I verified. Keyword absence from a document is not evidence of
absence from a codebase, and none of the remaining seven should be treated as a
finding until it too is traced to code.

## 5. Prioritised roadmap

Ordered by (measured impact) × (evidence strength) ÷ (risk to a working proxy).

### P0 — correctness gaps with measured consequences

**P0-1. In-flight quota reservation (closes F-B).**
Add a per-account in-flight counter and a locally-incremented usage estimate, so
selection sees `UsageCurrent + inFlight + sinceRefresh` rather than a value up to
30 min old. Decrement on completion with `defer`. Gate the account when the
projected value crosses `UsageLimit`, without waiting for the refresh tick.
- Acceptance: replay the 07-25 burst; the 112-cap sequence must reduce to at most a
  handful of caps before the account is gated.
- Risk: over-gating a healthy account. Mitigate by treating unknown quota as
  neutral (never as zero) and keeping the upstream refresh authoritative on arrival.

**P0-2. Cooldown on a monthly/request cap (closes F-C).**
Either wire the dead `MarkOverLimit` into the overage branch, or add an explicit
cap-specific cooldown until `NextResetDate`. A monthly cap is not a transient
condition and must not be retried in 7 seconds.
- Acceptance: after one cap, the same account is not re-dispatched within the
  cooldown; RED-prove with a test that fails when the cooldown is removed.
- Note: `setCooldownIfLater` must be used, never a raw assignment — round-11 already
  fixed a bug where a raw write shortened a longer backoff.

**P0-3. Reject silently-ignored request fields.**
Decide per field: implement, or reject with a clear 400. Silent acceptance is the
worst option because the client cannot tell. Start with the fields most likely to
change output semantics: `tool_choice`, `stop`/`stop_sequences`, `response_format`,
`top_k`, `reasoning_effort`.
- Risk: rejecting a field some client sends harmlessly today breaks it. Ship behind
  a config flag, default warn-only, and measure from the trace corpus which fields
  real clients actually send before enforcing.

**P0-4. `context.Context` propagation + graceful shutdown.**
Thread ctx from the client request into every upstream call; classify
`context.Canceled` as client-gone (never an account fault — round 13 established
exactly this contract for Bedrock, and it should hold everywhere); add
`Handler.Close(ctx)` and `signal.NotifyContext` + `srv.Shutdown` in `main.go`.
- Why it matters here: 94.5% of requests are agentic steps a client may abandon,
  and today an abandoned request keeps consuming upstream quota to completion.

### P1 — high-value, evidence-backed

**P1-1. Cap-dimension modelling.** Represent the AGENTIC_REQUEST cap explicitly and
fix the misleading "credits" comments on `UsageCurrent`/`UsageLimit`. Prerequisite
for P0-1 being legible to operators.

**P1-2. Filter → score → pick routing (closes F-D).** Promote health/latency from a
tie-break to a real scoring term, blending remaining quota, EWMA error rate, EWMA
latency, in-flight count, and affinity; keep LRU as a fairness component. Measured
headroom: 1.42x within-band TTFB spread with a choice available 68% of the time.
Enable quota-aware routing only *after* P0-1, or it will sort on stale numbers.

**P1-3. Prefix-affinity routing.** With 95.2% byte-level prefix repetition, route a
continuation to the account that served its parent turn. Kiro-Go already has
`apiKeyBinding` affinity; this extends it to conversation identity. Sell as
**latency**, not cost (see R1).

**P1-4. Close F-E: route Bedrock through `emitTrace`.** Own round, own RED tests.

### P2 — worth doing, weaker evidence

- Shared retry coordinator to replace the 6+ duplicated
  `for attempt < maxAccountRetryAttempts` loops, which have already diverged in
  trace handling and error mapping. Refactor of live dispatch — needs care.
- Admission control: global/per-account in-flight ceilings, 429 + `Retry-After`
  when saturated. Low urgency at 1 req/s; falls out of P0-1's counter cheaply.
- OTel export **as an optional build tag or sidecar exporter**, so the zero-dep
  invariant holds for the default binary (§6).
- Prompt-cache experiment on the Bedrock path (the only path where `cache_control`
  survives — Kiro-path caching was proven impossible in round 12 and re-confirmed
  this pass: all 938 "cache" occurrences across sampled bodies are free text in
  proxied prompts, zero structural cache keys).

### Explicitly NOT recommended

- **Redis / multi-replica shared state.** 0.45% of measured write ceiling. Adds a
  dependency and a failure mode to solve a problem this deployment does not have.
- **Embeddings / audio / images / batches endpoints.** Zero corpus demand.
- **Rewriting toward Envoy AI Gateway / Kubernetes.** Researcher 1's own
  recommendation is evolution, not a rewrite; Kiro-Go's protocol translators and
  account lifecycle are its differentiators.

## 6. Constraint that governs all of the above

CLAUDE.md pins **zero third-party dependencies beyond `github.com/google/uuid`**,
and `go.mod` confirms it at Go 1.21. Three roadmap items collide with that:
OTel export, Prometheus client libraries, and Redis. Each must either be
hand-rolled (as SigV4 and the AWS event-stream parser already are), gated behind a
build tag, or moved out of process. This is a real design constraint, not an
oversight — do not quietly add a dependency to satisfy a roadmap line.

Also note `/metrics` **404s in the live deployment by design**: it is gated behind
`MetricsEnabled` (default off) because the exposition is unauthenticated
(`config/config.go:454-460`). I initially recorded this as a defect; it is not.
