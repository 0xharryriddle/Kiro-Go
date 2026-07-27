# Plan — cache handling and test scenarios

Status: DRAFT for execution. Every factual claim below was verified against the
live tree or the running container by real command output; anything I could not
verify is labelled UNVERIFIED and is deliberately kept out of scope.

Base revision: `bc1efaa` on branch `harry`, working tree clean.

---

## 1. What actually exists today (verified)

Kiro-Go has **two unrelated cache subsystems**. Conflating them is the main
source of confusion, so they are separated here.

| # | Subsystem | File | Purpose | Bounded? | Observable? |
|---|---|---|---|---|---|
| A | **Prompt cache tracker** | `proxy/cache_tracker.go` | Derives Anthropic `cache_creation_input_tokens` / `cache_read_input_tokens` for the response it returns to the client | YES — LRU, capacity 131072, TTL prune, persisted to disk | YES — `/v1/stats` → `cache` |
| B | **Response cache (F5)** | `proxy/response_cache.go` | Exact-match whole-response cache; a hit avoids an upstream call entirely (saves a credit) | YES as of `bc1efaa` — LRU 2048 + expiry sweep | **NO** — no stats, no metrics, no admin surface |

### 1.1 The prompt cache has never run in production

Live evidence from the running container (`GET /v1/stats`):

```json
"cache": { "entries": 0, "capacity": 131072, "hits": 0,
           "misses": 0, "evictions": 0, "expirations": 0 }
```

with `totalRequests: 32563` and `totalTokens: 6218035569`.

Zero hits AND zero misses. `Compute()` increments one or the other on every call
(`cache_tracker.go:244-250`), so a zero/zero counter pair proves it was never
invoked — this is not a poor hit rate, it is a cold code path.

**Root cause (verified, two independent facts):**

1. `BuildClaudeProfile` returns a profile only when a block carries a cache
   breakpoint, and a breakpoint exists only where `extractPromptCacheTTL` finds
   an explicit `cache_control: {type: "ephemeral"}` on the request
   (`cache_tracker.go:204-214`, `:640-649`). No `cache_control` in the client
   request ⇒ `cacheProfile == nil` ⇒ tracker never consulted.
2. Even if a client did send it, the marker cannot reach Kiro:
   `KiroUserInputMessage.Content` is a plain `string` (`proxy/kiro.go:232`), so
   the Kiro wire format has no per-block structure to carry a marker.
   `cache_control` appears in `translator.go` and `cache_tracker.go` and in
   **zero** `kiro*.go` files.

So on the Kiro path the tracker is, at best, local bookkeeping — and today not
even that, because nothing triggers it.

### 1.2 Kiro DOES report real cache usage, and we discard it

`proxy/kiro.go:903-907` reads `uncachedInputTokens`, `cacheReadInputTokens`,
`cacheWriteInputTokens` from Kiro's own usage object. So upstream caching is a
real feature that reports real numbers.

But `handler.go:2617-2619` then overwrites the response usage with the
locally-synthesized `cacheUsage` unconditionally. When the tracker is cold
(always, today) it writes zeros over whatever upstream reported.

### 1.3 The Bedrock path has genuine, working prompt caching

`buildBedrockBody` (`proxy/bedrock.go:129-138`) deletes only `model` and
`stream`; everything else passes through. Verified by probe: all three
`cache_control` markers survive the rewrite intact, including `ttl: "1h"`.

Authoritative spec (fetched live, HTTP 200, AWS Bedrock user guide) confirms the
tracker's thresholds are correct: minimum 1024 tokens per checkpoint, 4096 for
Opus 4.5 / Opus 4.6 / Haiku 4.5 / Sonnet 4.5; TTL options 5 minutes and 1 hour.

---

## 2. Scope decision

Three candidate workstreams. Only the first two are in scope.

### IN SCOPE — W1: make the response cache trustworthy and observable

The response cache is the subsystem that actually saves credits, which is the
scarce resource this proxy exists to protect. It is now bounded but invisible:
an operator cannot tell whether it is helping, and a cache you cannot measure is
a cache you cannot tune or justify.

- W1.1 Add `responseCacheStats` (hits / misses / evictions / expirations /
  entries / capacity), mirroring `PromptCacheStats` exactly so the two read the
  same way.
- W1.2 Expose it at `/v1/stats` under `responseCache`.
- W1.3 Count a hit only when a body is actually served from cache; count a miss
  only when the request was cacheable and consulted the cache. Requests that
  were never cacheable must NOT count as misses, or the hit rate is meaningless.

### WITHDRAWN — W2: "stop discarding upstream cache numbers"

I scoped this as a correctness fix, then disproved my own premise while reading
the code. Recorded here rather than deleted, because the reasoning is the useful
part.

**What I assumed.** `proxy/kiro.go:903-907` reads `uncachedInputTokens`,
`cacheReadInputTokens` and `cacheWriteInputTokens` from Kiro's usage object, and
`handler.go:2623-2625` overwrites the response usage with locally-synthesized
values. That looked like real upstream numbers being thrown away.

**What is actually true.** Those three values are read inside
`updateTokensFromEvent`, whose signature is:

```go
func updateTokensFromEvent(event map[string]interface{},
    currentInputTokens, currentOutputTokens int) (int, int)
```

It returns two integers. The cache split is **summed away on the spot** —
`inputTokens = uncached + cacheRead + cacheWrite` — and never returned.
`cacheReadInputTokens` has exactly ONE non-test occurrence in the whole
repository, at that read site, and its single caller (`kiro.go:768`) captures
only the two totals.

So there is nothing at the response site to "stop overwriting with". The real
numbers do not reach the handler at all; they are destroyed one function earlier.

**Why I am not building the plumbing instead.** Threading a cache split out of
the stream parser is mechanically easy, but I cannot verify the values are ever
non-zero: `cacheReadInputTokens` appears in no captured response in `data/`, and
the live container reports `cache: {hits: 0, misses: 0}` after 32,563 requests.
Adding a data path to carry numbers I have never observed being populated would
be building on an assumption and calling it a fix. It also has the same root
cause as W3 — if Kiro never caches for us, these fields are always zero and the
plumbing is dead code.

**Status: SETTLED by captured traffic — see W3 below.** The "one capture would
unblock this" framing was wrong twice over: the captures already existed, and
what they show is that there is nothing to plumb.

### W3 — prompt caching on the Kiro path: PROVEN IMPOSSIBLE (not merely unverified)

Previously recorded as "UNVERIFIED, needs one captured Kiro IDE request body".
That blocker was self-imposed. The trace facility was already enabled
(`traceCaptureMode = redacted`, `traceMaxBodyBytes = 262144`) with **13,636
captured request/response bodies on disk** the entire time.

What the real outbound Kiro payloads contain, from 38 fully-parsed bodies (the
rest hit the 256 KiB cap and are truncated strings, so they parse only as text):

```
KiroPayload        keys → conversationState, inferenceConfig
conversationState  keys → chatTriggerType, conversationId, currentMessage, history
userInputMessage   keys → content, images, modelId, origin, userInputMessageContext
cache-related keys       → NONE at any level
```

A structural scan for `cache_control` / `cachePoint` / `cacheControl` /
`cacheReadInputTokens` / `cacheWriteInputTokens` / `uncachedInputTokens` as JSON
KEYS across 1,200 bodies found **zero**. Combined with
`KiroUserInputMessage.Content` being a plain `string` (no per-block structure to
attach a marker to), prompt caching cannot be expressed in this wire format.

This is now an empirical finding, not a deduction from the struct definition.

**FALSE-POSITIVE WARNING for anyone re-running this scan.** A naive text search
reported "262 of 600 bodies contain cache markers", including plausible-looking
hits for `cachePoint` and `cacheReadInputTokens`. Every one was contamination:
the captured conversations contain the *agent session that was investigating the
cache*, so the search matched its own shell commands quoted inside prompt text.
Exclude lines containing `grep`/`python3`/`re.compile`/`pat=` and require the
match to be a JSON key (`"name":`), or the corpus will confirm whatever you
search for.

**To reopen:** upstream documentation or a Kiro-IDE-originated capture from a
DIFFERENT client. Captures of our own traffic cannot answer it — they only ever
show what this proxy sends.

---

## 3. Test scenarios

Every scenario is RED-first: the test must fail against the pre-fix code, and
for the highest-value ones the fix is neutralized afterwards to confirm the test
goes red again. Positive controls are included so a test cannot pass merely by
asserting failure.

### 3.1 Response-cache stats (W1)

| ID | Scenario | Expected | Why it matters |
|---|---|---|---|
| T1 | Miss then hit on same key | `misses=1, hits=1` | Baseline correctness |
| T2 | Non-cacheable request (streaming / tools / thinking) | counters unchanged | A miss that was never eligible would corrupt the hit rate |
| T3 | Entry expires, same key requested | `misses=2`, not a hit | Expiry must not be silently reported as a hit |
| T4 | Overflow past capacity | `evictions > 0`, `entries == capacity` | Proves the bound is live, not decorative |
| T5 | Expiry sweep on insert | `expirations > 0` | Distinguishes swept-expired from LRU-evicted |
| T6 | `/v1/stats` exposes `responseCache` with all six fields | present and typed | Operator-facing contract |
| T7 | Two API keys, identical body | separate entries, no cross-tenant hit | Tenant isolation must survive the LRU rewrite |
| T8 | Concurrent Get/Set under `-race` | no race, counters monotonic | LRU list + map mutated together |

### 3.2 Upstream cache passthrough (W2) — NOT WRITTEN

T9–T12 were planned and are deliberately not written: W2 was withdrawn once the
premise was disproved (see §2). Writing tests for a data path that does not exist
would have meant building the path first on an unverified assumption.

If a capture ever shows a nonzero upstream `cacheReadInputTokens`, these are the
scenarios to write:

| ID | Scenario | Expected |
|---|---|---|
| T9 | upstream reports cacheRead=500, no local profile | response carries 500 |
| T10 | upstream silent | response carries 0, nothing fabricated |
| T11 | client sent `cache_control` | synthesized numbers keep priority |
| T12 | upstream reports cacheWrite only | maps to creation, read stays 0 |

### 3.3 Regression guards (existing behaviour that must not move)

| ID | Scenario | Expected |
|---|---|---|
| T13 | Full existing cache suite | all pass unchanged |
| T14 | `billedClaudeInputTokens` arithmetic | input + creation + read invariant holds |
| T15 | Prompt-cache LRU / TTL / persistence tests | all pass unchanged |

---

## 4. Execution order

1. W1.1–W1.3 with T1–T8 (RED → GREEN, neutralize-verified)
2. ~~W2~~ withdrawn — premise disproved, see §2
3. Full gate: `gofmt`, `go build`, `go vet`, full suite, `-race`, locale symmetry, `node --check`
4. Commit, push, confirm against the remote
5. Rebuild the container image, swap it, verify live: version reports 1.1.5, new symbols present, `/v1/stats` exposes `responseCache`

## 5. Rollback

- Code: previous commit `bc1efaa`, and the pre-existing image
  `sha256:c073c047fed4e3be6fbcfe7b7784fea8e91d315d4b55eabba1c258f4bfe4bc14`
  is retained locally, so a bad deploy is one `docker run` away from recovery.
- Both cache features remain config-gated and default OFF, so the blast radius
  of W1/W2 on a running deployment is limited to accounting fields.

## 6. Deliberately not doing

- No semantic / fuzzy cache. Exact-match only. A near-miss served as a hit is a
  wrong answer to a paying customer, and the design doc already rejected it.
- No cross-process / Redis cache. Single-instance is an explicit documented
  assumption; adding a shared store is a deployment change, not a code change.
- No cache of streaming, tool, or thinking requests. Those gates exist for
  correctness reasons that have not changed.
