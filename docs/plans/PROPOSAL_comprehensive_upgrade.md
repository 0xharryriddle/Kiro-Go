# Kiro-Go — comprehensive completion & upgrade proposal (round 17 audit pass)

## 0. How to read this document

This is a **whole-surface** audit, written after `cf8dab7` (round 16). It is the
companion to `ROADMAP_research_driven_upgrades.md`, which was driven by the *live
trace corpus* (workload evidence). This pass is driven by the **code and the
deployment surface** instead — build, lifecycle, CI, container, security posture,
maintainability — which is why it finds things the corpus could not show.

Every claim below carries its evidence class:

- **MEASURED** — a number produced by a command in this pass.
- **CODE-VERIFIED** — read at a cited `file:line`.
- **CANDIDATE DEFECT** — looks wrong, *not yet RED-proven*. Per this repo's
  methodology a defect is not real until a test fails without the fix. These are
  numbered `C-n` and must NOT be counted alongside the 78 proven defects.
- **DESIGN GAP** — works as written; the feature is simply absent.

No item here is claimed as fixed. Nothing in this document was deployed.

---

## 1. Verified baseline (MEASURED this pass)

| Dimension | Value |
|---|---|
| HEAD | `cf8dab7` (round 16), tree clean, `origin/harry` SHA-identical |
| Production Go source | **40,632 lines** across 6 packages |
| Test source | **32,646 lines** — a 0.80 test:src ratio |
| Top-level tests | **957** passing (`config` `pool` `auth` `proxy`), race clean |
| Proven defects closed | **78** across 16 rounds |
| Third-party deps | **1** (`github.com/google/uuid`), Go 1.21 |
| Largest file | `proxy/handler.go` at **8,198 lines** |
| Routes | **91** `case path ==` arms in one hand-rolled `ServeHTTP` switch |
| API-key secret storage | SHA-256 hashed + constant-time compare (already shipped) |

Package shape: `proxy` 56,947 total lines (62 src / 139 test files) — the centre of
mass; `auth` 5,925; `config` 5,448; `pool` 4,705; `logger` 141.

---

## 2. NEW findings this pass (absent from the existing roadmap)

### N-1. CI never compiles or tests the code — FIXED this pass (round 17)

**Status: closed.** `.github/workflows/ci.yml` added — two jobs, four gate steps
(`go build ./...`, `go vet ./...`, a `gofmt -l` check, and `go test ./... -race
-count=1`), pinned to Go 1.23 to match the Dockerfile builder stage that compiles
the shipped binary (not the local 1.25.6 toolchain).

RED-proven, each step independently, by temporarily introducing probe files and
observing a real failure — a gate that cannot fail is a false green:

| Step | Probe | Observed |
|---|---|---|
| `go vet` | `fmt.Printf("%s", int)` | exit 1, `wrong type int` |
| `gofmt -l` | misformatted spacing | exit 1, file listed |
| `go test` | `t.Fatal` in a probe test | exit 1, `--- FAIL` |
| `go build` | (n/a — covered by vet's compile) | exit 0 on clean tree |

Two corrections found while proving it, both of which would have shipped a weaker
gate:

1. My first probe was *accidentally well-formatted*, so the `gofmt` step passed and
   proved nothing. Rewrote the probe with real misformatting before claiming it.
2. The `go test` step initially failed on a **build** error from the vet-hostile
   probe, not on the failing test — so the test job's ability to report a genuine
   *test* failure was still unproven. Removed the vet probe and re-ran with only
   `t.Fatal` present to isolate it.

Baseline on the clean tree: build/vet/gofmt clean, and `go test ./... -race
-count=1` green in ~37s (auth 1.1s, config 2.3s, pool 2.7s, proxy 31.3s) — well
inside the 20-minute job timeout. Both probe files removed; tree verified restored.

The original finding, for the record:

#### Original finding (CODE-VERIFIED — highest-leverage gap)

`.github/workflows/` contains exactly four files: `docker.yml` and three issue
templates. `docker.yml` runs `docker/build-push-action` only.

**There is no `go build`, no `go vet`, no `go test`, no `gofmt` check in CI.**

32,646 lines of tests and 957 test functions — the single greatest asset in this
repo, built over 16 audit rounds — **run only when someone remembers to run them
locally**. The Docker build would catch a syntax error, but a failing test, a data
race, or a `gofmt` drift merges green.

Severity: this is the highest return-on-effort item in the entire document. It is
~40 lines of YAML and it protects everything else.

### N-2. No graceful shutdown — FIXED this pass (round 17)

**Status: closed.** `main.go` now runs `ListenAndServe` on its own goroutine under
`signal.NotifyContext(os.Interrupt, syscall.SIGTERM)`, drains via
`srv.Shutdown(ctx)` bounded by a new `shutdownGrace = 30s`, then calls a new
`Handler.Close()`. `proxy/shutdown.go` closes both stop channels, saves stats, and
flushes the prompt cache + trace store.

The half-built infrastructure this completes: `stopRefresh` and `stopStatsSaver`
already had **four readers** — `backgroundRefresh` (`handler.go:527`),
`importWatchLoop` (`import_watcher.go:71`), `backgroundStatsSaver` (`:2223`) and
`backgroundTracePrune` (`:2388`) — and **zero writers**. No `Close`/`Stop`/`Shutdown`
method existed on `Handler` at all. The loops could only ever die with the process.

Design points worth keeping:

- `Close()` must survive the **169 bare `&Handler{...}` literals** in the test
  suite, which leave stop channels and caches nil. Closing a nil channel panics and
  `promptCacheTracker.Stop` dereferences its receiver, so both are guarded.
  Idempotency uses a `closeState` struct wrapping `sync.Once`, so a zero value is
  usable and no existing test literal needed editing.
- `Close()` runs **after** the drain, so requests completing during shutdown are
  included in the final stats save.
- A **second** signal restores default handling (`stop()` is called once the drain
  begins), so an operator can always force-kill instead of waiting out the grace
  period.
- 30s is a compromise, documented as such: SSE streams here can run for minutes, so
  no realistic deadline guarantees completion, and waiting forever would hang a
  deploy behind one slow client. Docker's default 10s SIGKILL timeout will usually
  cut it shorter anyway — `stop_grace_period` must be raised if a full drain
  matters.

**A real bug in my own `Close()`, caught by my own test.** The first RED run did not
fail on the assertion — it **segfaulted**: `config.UpdateStats` (`config.go:1865`)
dereferences `cfg` with no nil guard, and `Close()` is the first caller that can run
before `Init` succeeds. Two consequences, the second worse than the first:

1. A crash during shutdown whenever `Init` never ran or failed.
2. `Save()` would marshal a nil `cfg` to the 4-byte literal `null` —
   **verified empirically**, `json.MarshalIndent` returns `("null", nil)` — which is
   non-empty and therefore passes `atomicWriteConfig`'s empty-write refusal,
   clobbering a real config file with `null`.

Fixed at the source in `config.UpdateStats`. Note the convention this respects: 19
config *readers* nil-guard `cfg`, writers historically did not, because every writer
ran after a successful `Init`. A shutdown hook is the first caller for which that
assumption no longer holds. **Not counted among the 78 proven defects** — it was
unreachable in shipped code and only became reachable via this new path.

RED-proof: `proxy/shutdown_test.go`, 6 tests. Under neutralization (Close reduced to
its pre-fix no-op) exactly the 3 behavioural tests fail — both channel-selector
tests hang to their 2s deadline and the prompt-cache flush never lands — while all 3
controls (idempotency, bare-literal survival, nil receiver) stay green. Restored
byte-identical, sha verified.

**End-to-end proof, not just unit tests.** Built to `/tmp`, ran with an isolated
`CONFIG_PATH` on port 18099, confirmed `/healthz` served, sent a real `SIGTERM`, and
observed the full ordered sequence in the process log:

```
Shutdown signal received; draining in-flight requests (up to 30s)
HTTP server drained cleanly
Handler closed: background loops stopped, state flushed
Shutdown complete
```

Process exited 0. The live `data/config.json` (24 real accounts) was verified
byte-identical before and after, since the e2e wrote only to its isolated path.

The original finding, for the record:

#### Original finding (CODE-VERIFIED)

`main.go:109` calls `srv.ListenAndServe()` and nothing else. MEASURED across the
tree: `signal.Notify` = **0 occurrences**, `.Shutdown(` = **0 occurrences**.

On `docker compose up -d`, SIGTERM kills the process immediately. Every in-flight
SSE stream is severed mid-token. Worse, given the billing model: work already
consumed upstream may never be recorded, because the settlement path never runs.

The roadmap lists this inside P0-4 bundled with ctx propagation. It should be split
out — `signal.NotifyContext` + `srv.Shutdown(ctx)` is roughly 15 lines and carries
almost no risk, whereas full ctx threading is a large refactor. Do not let the
cheap half wait on the expensive half.

### N-3. Client cancellation is not propagated upstream (MEASURED)

`http.NewRequest(` = **32 non-test call sites**. `http.NewRequestWithContext(` = **1**.

So when a client abandons an agentic step — and per the corpus 94.5% of traffic is
agentic steps — the upstream Kiro/Bedrock call keeps running to completion, burning
quota nobody will read. Round 13 established the correct contract (`clientGone` is
never an account fault); this extends that from *billing* to *actually stopping the
work*.

### N-4. The customer hot path reads request bodies with no size cap — FIXED this pass (round 17c)

**Status: closed, together with C-1 below.** `config.GetMaxRequestBodyBytes()` (new,
default 32 MiB, clamped up from anything under 64 KiB) plus a new
`proxy/request_body_limit.go` helper now bound all four customer surfaces, and
`apiImportCliJson` was given the same 1 MiB cap its four siblings already had.

**A number in my own text, corrected.** The line below originally read "13
`MaxBytesReader` sites vs **9** bare `io.ReadAll(r.Body)`", which contradicted the
five-row table directly beneath it. Re-measured: there are **9 `io.ReadAll(r.Body)`
occurrences**, but **4 of them are already preceded by a `MaxBytesReader` assignment
on the line above** (`apiImportCredentials:5997`, `apiPreviewCredentials:6612`,
`apiApplyCredentials:6641`, `apiPreviewCliJson:6719`). So the unguarded count was
**5**, not 9 — the table was right and the prose was wrong. The distinction matters
because "9 unguarded sites" would have sent a later reader to re-guard four sites
that were already correct.

MEASURED, corrected: 13 `MaxBytesReader` sites vs 9 `io.ReadAll(r.Body)` sites, of
which **5 were unguarded**. Those five, with their enclosing functions:

| Site | Function | Exposure |
|---|---|---|
| `handler.go:1557` | `handleClaudeMessagesInternal` | **customer hot path** |
| `handler.go:1515` | `handleCountTokens` | customer |
| `handler.go:2767` | `handleOpenAIChat` | **customer hot path** |
| `responses_handler.go:21` | `handleOpenAIResponses` | customer |
| `handler.go:6783` | `apiImportCliJson` | admin |

Confirmed absent upstream too: no `MaxBytesReader` and no `ContentLength` check in
`proxy/auth.go`, and none in the `ServeHTTP` prologue (`handler.go:748-775`). So
nothing bounds these. `ReadTimeout: 60s` bounds *duration*, not *size* — a fast
client can stream gigabytes into `io.ReadAll` inside the timeout and the process
holds it all in memory at once.

This interacts badly with the documented posture that `requireApiKey = false` and
"the trust boundary is the network": anyone who can reach the port can OOM the proxy
with a single request, with no credential.

### C-1. `apiImportCliJson` was the only credential importer without a body cap — FIXED (round 17c)

Its four siblings all cap at 1 MiB on the line immediately before the read —
`apiImportCredentials:5997`, `apiPreviewCredentials:6612`, `apiApplyCredentials:6641`,
`apiPreviewCliJson:6719`. `apiImportCliJson:6787` did not.

Four out of five siblings sharing a guard, with the fifth missing it, read as an
oversight rather than a decision, and it was: `apiImportCliJson` now takes the same
`1<<20` bound as `apiPreviewCliJson`, the preview half of its own pair. Kept at the
admin limit rather than routed through the new customer helper, because these two
endpoints are one feature and should refuse at the same size.

Filed as a hardening item, not as defect #79: an admin credential is required to
reach it, and — unlike the customer surfaces — no unauthenticated path exists.

### How N-4 / C-1 were verified

`proxy/request_body_limit.go` wraps `http.MaxBytesReader` and returns a sentinel so
each surface can answer **413** in its own error dialect (`request_too_large`, the
type `account_failover.go:115` already uses for an upstream 413) instead of the 400
it would send for an unreadable body. Wrapping the reader rather than trusting
`Content-Length` is deliberate and matches the convention already documented in
`admin_bot_api.go`: a chunked or lying `Content-Length` cannot slip past a reader
that counts actual bytes.

**The 32 MiB default is an inference, and is labelled as one.** The corpus cannot
measure customer body size directly — it stores the *rewritten upstream* body,
truncated at `TraceMaxBodyBytes` (256 KiB), and among non-truncated records the
stored request is usually empty (p50 = 0 B/token), so bytes-per-token cannot be
calibrated from it. What it *can* measure: across 22,855 billed requests the p99
prompt is **779,709 tokens** and the maximum **903,947**. At a deliberately
pessimistic 12 B/token (the highest ratio seen on any non-truncated record) the
largest real request lands near 10 MiB, so 32 MiB leaves ~3x headroom over observed
peak traffic. Rejection rates at candidate caps, computed on that corpus: 1 MiB
would reject **32.0%** of real requests, 2 MiB **4.6%**, 4 MiB and above **0%**. A
cap chosen by intuition would plausibly have been 1 MiB — an outage.

RED-proof: `proxy/request_body_limit_test.go` (5 tests) + `config/request_body_limit_test.go`
(2 tests). Under neutralization of the proxy helper, all 4 behavioural subtests fail
and all 3 controls stay green; under neutralization of the config clamp, both config
tests fail. Both files restored byte-identical, sha verified.

**A test-design correction worth keeping.** The first RED run did not fail an
assertion — it **segfaulted**. Pre-fix, an oversized body is buffered and the handler
proceeds to dispatch, where a bare test `Handler` has a nil pool. That aborted the
whole test binary, so the sibling subtests never ran and the neutralization result
was unreadable — the failure looked like a broken test rather than a proven defect.
The assertion now runs the handler under a `recover()` that records a panic *as* the
failure, because reaching dispatch at all is precisely the defect. Lesson:
**a RED signal that crashes the runner is not a usable RED signal.**

### N-5. The admin surface has no brute-force protection (CODE-VERIFIED)

`authenticateAdminKey` (`proxy/admin_bot_api.go:64-83`) does a single
`subtle.ConstantTimeCompare` against `config.GetPassword()`. Correct as far as it
goes — constant-time, and it rejects the empty-password case.

But MEASURED: zero occurrences of `rateLimiter`, `Admit`, `lockout`, or
`failedAttempt` anywhere in `admin_bot_api.go`. The per-key `rateLimiter` exists and
is wired to *customer* keys only (`proxy/auth.go:100`).

So the admin password — one shared static secret guarding account creation, key
minting, and credit recharge — accepts unlimited guesses at full line rate, with no
delay, no lockout, and no alert. A per-IP failure counter with exponential backoff
plus a webhook alert on repeated failures is a small, contained change.

### N-6. No TLS support in-process (CODE-VERIFIED)

`ListenAndServeTLS` = **0 occurrences**. The proxy is plaintext-only and therefore
hard-depends on an external terminator. That is a legitimate design choice, but it
is undocumented — and combined with N-5, a misconfigured deployment ships a static
admin password over cleartext HTTP. At minimum this belongs in the README as an
explicit deployment requirement; optionally add `TLSCertFile`/`TLSKeyFile` config.

### N-7. Container runs as root with no image-level healthcheck (CODE-VERIFIED)

`Dockerfile` has **no `USER`** directive → the process runs as root, and `VOLUME
/app/data` means root-owned writes to the mounted config. It also has **no
`HEALTHCHECK`**; `docker-compose.yml:53` supplies one, so anyone running the image
directly (not via compose) gets no health signalling. Adding a non-root `USER` plus
an image-level `HEALTHCHECK` is standard hardening.

### N-8. `proxy/handler.go` is an 8,198-line file routing 91 endpoints (MEASURED)

Not a bug — a maintainability ceiling, and it is now actively slowing this audit
programme. Every round pays a re-reading tax on it, and the 91-arm `ServeHTTP`
switch orders arms by *specificity*, so a new route inserted in the wrong place is
silently shadowed (the file already carries a comment warning about exactly this
hazard above the `/admin/` static-file arm).

Proposal: split by surface into `handler_claude.go`, `handler_openai.go`,
`handler_admin_api.go`, `handler_health.go`, and lift routing into an explicit table
(`method + path + auth + handler`) walked by one loop. Behaviour-preserving,
mechanical, and it makes shadowing a *data* error instead of an ordering error.
Do it AFTER N-1, so CI is guarding the refactor.

---

## 2b. Claims I wrote and then refuted in this same pass

Two items in my first draft of §3 Track C were copied from CLAUDE.md's "Known scope
gaps" section rather than checked against code. Probing them showed **both are
stale**. Recording the correction here rather than quietly deleting it, because the
same stale notes will mislead the next reader of CLAUDE.md.

### Refuted: "the OpenAI surface is not wired for Bedrock"

CLAUDE.md:89-90 states: *"OpenAI-compatible surface is not wired for Bedrock (needs
OpenAI→Anthropic request translation). Bedrock accounts are currently skipped in the
OpenAI loops."*

CODE-VERIFIED false for `/v1/chat/completions`. The translation layer exists and is
wired:

- `proxy/bedrock_openai.go` (794 lines) implements the full bidirectional bridge —
  `openAIToAnthropicMessages` (`:92`), `anthropicMessageToOpenAIResponse` (`:442`),
  a streaming converter (`newBedrockOpenAIStreamConv`, `:563`), plus tool-choice,
  stop-sequence, and image-block translation.
- It is dispatched from the live OpenAI loops: `handleOpenAIStream` calls
  `invokeBedrockOpenAIStream` at `handler.go:2873`, and `handleOpenAINonStream`
  calls `invokeBedrockOpenAINonStream` at `handler.go:3346`.

So `/v1/chat/completions` serves Bedrock accounts today. Only `/v1/responses` does
not, and that is a genuine, narrower gap (C2 above). The `IsBedrock()` skip in
`responses_handler.go` is correct-by-design, not the same defect.

### Refuted: "Bedrock model auto-discovery would remove the guesswork"

CLAUDE.md:91-93 proposes `bedrock:ListFoundationModels` auto-discovery as a
follow-up. It is **already implemented and wired**:

- `proxy/bedrock_discovery.go` (415 lines) calls `/foundation-models`
  (`discoverBedrockModels`, `:257`), filters to ACTIVE on-demand text models
  (`onDemandTextModelIDs`, `:100`), merges inference profiles (`:138`), and caches
  per account with a TTL (`:180-215`) behind a per-account mutex (`:325`).
- The resolution path consults it: `resolveBedrockModelID` (`proxy/bedrock.go`)
  tries the account's explicit `BedrockModelMap` (`:94`), then
  `discoveredBedrockModelFor` (`:111`), and only then falls back to
  `defaultBedrockModelMap` (`:119`).
- Also wired into `fetchAndCacheAccountModels` (`handler.go:1250`),
  `apiGetAccountModels` (`:7720`), and region selection
  (`bedrock_region.go:284`).

The convenience defaults are now a *last-resort fallback behind* live discovery, not
the primary mechanism. Nothing to build.

**Action item:** CLAUDE.md's "Known scope gaps" section is stale on both counts and
should be corrected, since it is the first thing an agent reads before touching
Bedrock code.

---

## 3. Feature proposal by track

### Track A — Correctness & safety (do these first)

- **A1. CI gate** (N-1) — **DONE, round 17.** `.github/workflows/ci.yml`:
  `go build ./...`, `go vet ./...`, `gofmt -l` check, and `go test ./... -race
  -count=1` on push and PR, pinned to Go 1.23 (Dockerfile builder parity).
  Concurrency-cancelled per ref, `permissions: contents: read`. Each step
  RED-proven — see N-1.
- **A2. Graceful shutdown** (N-2) — **DONE, round 17b** (`28cb891`).
  `signal.NotifyContext` + `srv.Shutdown(ctx)` bounded by `shutdownGrace = 30s` +
  a new `Handler.Close()` that stops both background loops and flushes stats,
  prompt cache and trace store. Verified end-to-end with a real `SIGTERM` — see N-2.
- **A3. Body-size ceilings** (N-4, C-1) — **DONE, round 17c.** One helper
  (`proxy/request_body_limit.go`) applied at the four customer entry points, plus
  the 1 MiB sibling cap on the one unguarded importer. Cap configurable via
  `maxRequestBodyBytes`, default 32 MiB (~3x observed peak, 0% of real traffic
  rejected), clamped up from anything under 64 KiB. Returns 413 `request_too_large`
  in each surface's own error dialect — see N-4.
- **A4. Admin brute-force resistance** (N-5): per-IP failure counter, exponential
  backoff, webhook alert on threshold.
- **A5. Context propagation** (N-3): thread `r.Context()` into all 32 upstream call
  sites; classify `context.Canceled` as client-gone, never an account fault.
  Large — stage it per surface, Claude path first.

### Track B — Routing & efficiency (roadmap-aligned, evidence already gathered)

- **B1. In-flight quota reservation** — roadmap P0-1. The single largest measured
  waste: 30-minute stale quota produced 112 cap errors in 8 minutes.
- **B2. Cap-aware backoff sizing** — finish round 16's other half: size the overage
  backoff from `NextResetDate` instead of a flat 1h.
- **B3. Filter → score → pick routing** — roadmap P1-2. 1.42x within-band TTFB
  spread, choice available 68% of the time.
- **B4. Prefix-affinity routing** — roadmap P1-3. 95.2% prefix repetition. Sell as
  latency, not cost (R1 refuted the cost story).
- **B5. Admission control** — global + per-account in-flight ceilings, 429 with
  `Retry-After`. Falls out of B1's counter nearly free.

### Track C — Protocol surface completeness

- **C1. Reject silently-ignored fields** — roadmap P0-3. Ship warn-only behind a
  flag first; measure from the corpus before enforcing.
- **C2. `/v1/responses` cannot serve a Bedrock account** — narrowed after probing;
  see §2b, which corrects two claims I initially wrote here from stale CLAUDE.md
  notes. `/v1/chat/completions` **already works** for Bedrock. Only `/v1/responses`
  is unserved: round 14 correctly made it *skip* Bedrock accounts rather than
  penalise them (`responses_handler.go:214`, `:443`), but skip means a
  Bedrock-only pool answers `/v1/responses` with no eligible account. Closing this
  needs an OpenAI-Responses→Anthropic translation, which does not exist yet.
- **C3. Bedrock model auto-discovery — ALREADY SHIPPED, do not build.** See §2b.

### Track D — Observability & operations

- **D1. Bedrock paths emit no structured trace row** — roadmap F-E / P1-4.
- **D2. Metrics coverage audit** — `/metrics` exists, is Prometheus-shaped, and is
  gated behind `MetricsEnabled` (default off, 404 when disabled, no auth — the
  standard scrape model, documented at `metrics_prometheus.go:158`). Audit *which*
  series exist against the routing decisions operators actually need to debug.
- **D3. OTel export behind a build tag** — roadmap P2, constrained by the zero-dep
  invariant (§6).

### Track E — Security posture

- **E1. TLS documentation or support** (N-6).
- **E2. Container hardening** (N-7): non-root `USER`, image `HEALTHCHECK`.
- **E3. Secrets at rest** — CLAUDE.md's own known gap: Bedrock IAM secrets and OAuth
  tokens sit plaintext in `config.json`. Customer API keys are already hashed, so
  the pattern to follow exists. Repo-wide change; sequence it deliberately.
- **E4. Admin auth beyond one shared password** — scoped admin tokens with
  per-token audit trail. Larger; only after A4.

### Track F — Maintainability

- **F1. Split `handler.go` + table-driven routing** (N-8).
- **F2. Shared retry coordinator** — roadmap P2; 6+ duplicated retry loops that have
  already diverged in trace handling and error mapping.
- **F3. Continue the audit rounds** on still-unreviewed large files:
  `admin_bot_api.go` (1,441 lines), `request_trace_recorder.go` (483),
  `kiro_api.go` (1,520), `translator.go` (2,955).

---

## 4. Already shipped — do NOT rebuild

Verified present in this pass, listed because they are easy to re-propose:

- API-key secrets hashed (SHA-256) with constant-time comparison and key masking.
- Per-key RPM/TPM rate limiting, with the `estTokens=0` admission bypass already
  closed (`rate_limiter.go:85` — the "already at budget" branch fires independently
  of the estimate).
- Prometheus `/metrics`, `/healthz`, `/readyz` (the latter checks config validity,
  writability, and web assets).
- Body caps on 13 sites including 4 of 5 credential importers.
- Webhook notifications, trace capture with retention + body caps, prompt-cache
  config surface, multi-region Bedrock support, Microsoft 365 SSO.

---

## 5. Explicitly NOT recommended

Carried forward from the roadmap's evidence, restated so it is not re-litigated:

- **Redis / multi-replica shared state** — measured at 0.45% of the write ceiling.
- **Embeddings / audio / images / batches endpoints** — zero corpus demand.
- **Rewriting toward Envoy AI Gateway / Kubernetes** — the researchers' own
  recommendation was evolution, not rewrite.
- **Kiro-path prompt caching** — proven impossible twice (rounds 12 and 14).

---

## 6. Recommended sequence

Ordered by (impact × evidence) ÷ risk, with cheap-and-safe pulled forward:

1. ~~**A1 CI gate**~~ — **DONE, round 17a** (`60fa604`). Smallest diff, now protects
   all 970 tests on every push and PR.
2. ~~**A2 graceful shutdown**~~ — **DONE, round 17b** (`28cb891`). Ends mid-stream
   kills on deploy; also closed a latent `null`-clobber hazard in `UpdateStats`.
3. ~~**A3 body caps + C-1**~~ — **DONE, round 17c.** Closed an unauthenticated
   memory-DoS surface on all four customer entry points.
4. **A4 admin brute-force** — small, closes an unlimited-guess hole. **Next up.**
5. **B1 in-flight quota** — largest measured efficiency win; unlocks B3.
6. **B2 cap backoff sizing** — finishes round 16 honestly.
7. **F1 handler split** — unblocks every later round; safe once CI guards it.
8. **A5 ctx propagation**, then B3/B4, then Track C/D/E by need.

Items 1-4 are individually small and mutually independent — they can land as four
tight rounds without destabilising dispatch. Items 5-8 touch live routing and each
deserve its own RED-proven round.

## 7. Governing constraint

`CLAUDE.md` pins **zero third-party dependencies beyond `github.com/google/uuid`**,
confirmed by `go.mod` at Go 1.21. This is why OTel (D3) must be a build tag or a
sidecar, why Prometheus stays hand-rolled, and why Redis is out. Any proposal that
requires a new dependency must state that cost explicitly and get a decision — not
quietly add it.
