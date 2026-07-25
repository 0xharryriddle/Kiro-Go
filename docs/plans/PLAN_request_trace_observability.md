# Request Trace Observability Implementation Plan

> **For Hermes:** implement task-by-task. Each task is 2-5 minutes, TDD, commit per task.

**Goal:** Turn the Logs tab from an 11-column summary table into a full request tracing
surface where an operator can select any single request and see exactly what Kiro-Go
received, what it sent upstream, which accounts/regions/profiles it tried, what came
back, how long each stage took, and why it failed.

**Architecture:** Keep the existing `RequestLog` name and JSON shape as the metadata
record (additive `omitempty` fields only, so existing `data/request_logs.json` still
loads). Add an attempt-level child array so account failover becomes visible. Move
persistence from "re-marshal the whole 500-entry array on every request" to an
append-only JSONL log with rotation. Store request/response bodies in a separate,
opt-in, TTL-pruned blob tier referenced by trace ID, so the default privacy posture
does not change. Field naming follows the OpenTelemetry GenAI semantic conventions
where a direct equivalent exists.

**Tech Stack:** Go 1.21, stdlib only (`go.mod` has exactly one dependency,
`github.com/google/uuid v1.6.0` — no new modules). Frontend is vanilla JS
(`web/app.js`), no framework, no build step. i18n via `web/locales/{en,zh}.json`.

---

## 1. Audit findings (verified against current bytes)

Every item below was confirmed by reading the code, not inferred.

### 1.1 The trace identifier is dead code

`RequestLog.RequestID` is declared at `proxy/handler.go:38` and is **never assigned
anywhere in the repository**. Both constructors (`recordFailureWithDetails`
`proxy/handler.go:1669`, `recordSuccessLog` `proxy/handler.go:1685`) omit it. So there
is no key to correlate a log row with anything — not a client retry, not an upstream
call, not a body.

### 1.2 Failover attempts are invisible

`handleClaudeNonStream` / `handleClaudeStream` / the OpenAI and Responses equivalents
loop up to `maxAccountRetryAttempts` accounts. Inside the loop
(`proxy/handler.go:1510-1520`) a failed attempt does:

    lastErr = err
    excluded[account.ID] = true
    h.handleAccountFailure(account, err)
    if !messageStarted {
        continue
    }
    h.recordFailureWithDetails("claude", model, account.ID, err)

The `continue` fires **before** the log call on the common path (nothing streamed
yet), so every intermediate account failure is discarded. Only the terminal error is
logged, once, at `proxy/handler.go:1578`. An operator sees "1 error" for a request
that actually burned four accounts. This is the single biggest blind spot for a
multi-account pool.

Same shape at `proxy/handler.go:1909`, `2385`, `2508`,
`proxy/responses_handler.go:169`, `478`.

### 1.3 Endpoint fan-out inside a single attempt is not logged at all

`CallKiroAPI` (`proxy/kiro.go:419-483`) iterates `getSortedEndpoints(...)`, and on
non-200 sets `lastErr` and `continue`s. Those per-endpoint failures only ever reach
`logger.Warnf` (`proxy/kiro.go:476`) — stdout text, not the Logs feature.

### 1.4 Cache hits never appear in the logs

`proxy/handler.go:1112-1122`: on a response-cache hit the handler writes the cached
body, calls `recordSuccessForApiKey` (which bumps counters and rate windows), then
returns. It never calls `recordSuccessLog`. The request is billed and rate-limited but
absent from the Logs tab, so `logCount` and `totalRequests` disagree with no
explanation.

### 1.5 Rejected traffic is never logged

- Auth failures return `authError` at `proxy/auth.go:76`, `80`, `83`, `112`, `115`.
- Rate-limit rejections return at `proxy/rate_limiter.go:103` (rpm) and `114` (tpm).

Neither path produces a `RequestLog`. A tenant hammering with a bad key, or getting
429'd all day, generates zero trace evidence.

### 1.6 Upstream diagnostics are read and thrown away

`CallKiroAPI` reads exactly one response header, `Retry-After`
(`proxy/kiro.go:458`). Every other upstream response header is discarded, so if Kiro
returns an AWS-style correlation ID (`x-amzn-RequestId` / `x-amzn-requestid` is the
usual shape for AWS APIs — UNVERIFIED here, nothing in this repo reads it), we throw
it away and a Kiro/AWS support escalation has no correlation ID. Task 6 captures the
whole response header allow-list rather than betting on one name, and its first step
is to log what the live endpoint actually returns.

The error body is read at `proxy/kiro.go:469` and flattened into an error string —
HTTP status code is lost as a discrete field (it survives only as text inside
`error`).

### 1.7 No bodies, no shapes, no timing detail

The record has 11 scalar fields. There is no request payload, no response payload, no
system-prompt shape, no tool-call list, no stop reason, no HTTP status, no
input/output token split (`Tokens` is a single collapsed sum), no region, no profile
ARN, and no time-to-first-byte. `reqStart` exists in every handler and only total
duration is derived. For a proxy that just grew multi-region/multi-profile routing,
not logging region and profile ARN is a serious omission.

### 1.8 Persistence is O(n) per request and races on a shared temp path

`appendRequestLog` (`proxy/handler.go:1712-1724`) copies the entire ring buffer and
spawns `go persistRequestLogs(snapshot)` on **every single request**.
`persistRequestLogs` (`1744`) then `json.MarshalIndent`s all 500 records and rewrites
the file. Two costs:

1. Every request re-serialises the whole history — indented, so ~2x larger.
2. All writers share one temp path, `requestLogsPath + ".tmp"`
   (`proxy/handler.go:1757`). Concurrent goroutines `os.WriteFile` the same path and
   then `os.Rename` it. The rename is atomic, but two overlapping writers can
   interleave content into that single temp file before either rename lands, so the
   promoted file can be torn. Unique temp names or a serialised writer is required.

`data/request_logs.json` is `0600` and `data/` is `0755` — that part is correct and
should be preserved.

### 1.9 Query API is a linear substring scan with no windowing

`filterRequestLogs` (`proxy/handler.go:4934`) supports `status` and a lowercase
substring `q` across concatenated fields. `apiGetLogs` (`4959`) calls it with
`limit` hardcoded to `0`, so the `limit` query parameter is **not supported at all**.
There is no time range, no pagination/cursor, no sort control, and no field-scoped
filter (e.g. only-this-account, only-this-model). Every call to `getRequestLogs`
(`4895`, `4961`, plus `proxy/admin_account_health.go:36` and
`proxy/admin_usage_anomaly.go:29`) deep-copies all 500 records.

### 1.10 Clearing logs leaves no audit trail

`apiClearLogs` (`proxy/handler.go:4997-5003`) truncates the buffer and
`os.Remove`s the file without writing an `AuditLog` entry — the one action that
destroys evidence is itself unrecorded, while far more benign actions are audited.

### 1.11 Frontend is a flat non-interactive table

`renderLogs` (`web/app.js:761-817`) builds one `<table>` via string concatenation with
8 columns, no row expansion, no detail view, no pagination, no time filter. The full
error string is dumped into a `title` attribute (`web/app.js:800`). Controls in
`web/index.html:781-796` are: status select, search box, 2 export buttons, refresh,
clear. 29 `logs.*` and 4 `metrics.*` locale keys exist today (778 total leaves).

---

## 2. Design

### 2.1 Record model

Three tiers, so cost scales with what the operator actually asks for.

| Tier | Content | Default | Where |
|---|---|---|---|
| Index | one metadata line per request | always on | `data/traces/index-YYYYMMDD.jsonl` |
| Attempts | per-account/per-endpoint child rows | always on | embedded in the index line |
| Bodies | request/response payloads | **off** | `data/traces/bodies/YYYYMMDD/<traceID>.json.gz` |

`RequestLog` keeps its name and every existing field with identical JSON tags, so old
files deserialise unchanged. All new fields are `omitempty`.

### 2.2 Capture modes

`traceCaptureMode` in `data/config.json`, overridable by `TRACE_CAPTURE_MODE`:

- `off` — no trace records beyond today's behaviour.
- `meta` — **default.** Index + attempts. No prompt or response text ever. Matches the
  current privacy posture, so upgrading changes nothing about what is stored.
- `redacted` — bodies stored with `redactPII` (`proxy/translator.go:464`) plus secret
  scrubbing applied.
- `full` — bodies stored verbatim. Requires an explicit second flag,
  `traceCaptureAcknowledgeRisk: true`, or it silently degrades to `redacted`.

This mirrors the OTel GenAI stance: `gen_ai.input.messages` and
`gen_ai.output.messages` are classified `Opt-In` precisely because they are "likely to
contain sensitive information including user/PII data". Never default to storing
prompts.

### 2.3 Secret scrubbing (mandatory in every mode above `off`)

Bodies and headers pass a deny-list before they are written: `Authorization`,
`X-Api-Key`, `Cookie`, `Set-Cookie`, any `ksk_[A-Za-z0-9]+`, `accessToken`,
`refreshToken`, `idToken`, `clientSecret`, `Bearer <token>`. Header capture is
allow-list only (`content-type`, `accept`, `user-agent`, `x-amzn-*`, `retry-after`,
`x-kiro-cache`) — never a blanket header dump.

### 2.4 Field naming

Where an OTel GenAI attribute exists, the JSON field is a camelCase transliteration so
a future exporter is mechanical:

| Trace field | OTel GenAI attribute |
|---|---|
| `model` | `gen_ai.request.model` |
| `responseModel` | `gen_ai.response.model` |
| `inputTokens` | `gen_ai.usage.input_tokens` |
| `outputTokens` | `gen_ai.usage.output_tokens` |
| `stopReason` | `gen_ai.response.finish_reasons` |
| `ttfbMs` | `gen_ai.server.time_to_first_token` |
| `durationMs` | `gen_ai.server.request.duration` |
| `stream` | `gen_ai.request.stream` |
| `errorType` | `error.type` |
| `upstreamHost` | `server.address` |

### 2.5 Retention

`traceRetentionHours` (default 168 = 7 days) and `traceMaxIndexBytes` (default 256MB)
prune by whole rotated file, oldest first, on a ticker. Bodies prune on the same clock.
The in-memory ring stays at 500 for the live view; history comes off disk.

---

## Phase 0 — Trace identity and the silent-drop fixes

Highest value per line. No schema explosion, no storage change.

### Task 0.1: Assign a trace ID

**Files:** Create `proxy/request_trace.go`; test `proxy/request_trace_test.go`.

**Step 1: failing test**

```go
package proxy

import "testing"

func TestNewTraceIDIsUniqueAndPrefixed(t *testing.T) {
	a := newTraceID()
	b := newTraceID()
	if a == b {
		t.Fatal("trace IDs must be unique")
	}
	if len(a) < 8 || a[:4] != "trc_" {
		t.Fatalf("unexpected trace id shape: %q", a)
	}
}
```

**Step 2:** `go test ./proxy/ -run TestNewTraceIDIsUniqueAndPrefixed` — expect FAIL,
undefined: newTraceID.

**Step 3: implement**

```go
package proxy

import "github.com/google/uuid"

// newTraceID returns a collision-free identifier for one client request.
func newTraceID() string {
	return "trc_" + uuid.New().String()
}
```

**Step 4:** rerun — PASS. **Step 5:** commit.

### Task 0.2: Thread the trace ID through one handler

**Files:** Modify `proxy/handler.go` (`handleClaudeMessagesInternal` ~1063).

Mint the ID once per client request, store it on a small struct, and pass it into the
record constructors. Signature change is mechanical:
`recordSuccessLog(traceID, endpoint, model, accountID string, ...)`. Update all 8 call
sites listed in 1.2. Set `RequestID` (the existing dead field) to the trace ID so no
new field is needed for tier 1.

Verify: `go build ./...`, then `grep -rn "RequestID:" proxy/` must show a non-empty
assignment.

### Task 0.3: Log every failover attempt (fixes 1.2)

**Files:** Modify `proxy/handler.go`; create `proxy/request_trace_attempts_test.go`.

**Step 1: failing test** — drive the loop with a stub that fails twice then succeeds,
assert the resulting record has `AttemptCount == 3` and `len(Attempts) == 3` with the
first two carrying distinct account IDs.

**Step 2:** run — FAIL (attempts always length 0).

**Step 3:** implement `TraceAttempt` and accumulate one per loop iteration, before the
`continue`:

```go
// TraceAttempt is one upstream dispatch attempt within a single client request.
// A request that fails over across three accounts produces three attempts.
type TraceAttempt struct {
	Seq               int    `json:"seq"`
	AccountID         string `json:"accountId,omitempty"`
	AccountEmail      string `json:"accountEmail,omitempty"`
	UpstreamEndpoint  string `json:"upstreamEndpoint,omitempty"`
	UpstreamHost      string `json:"upstreamHost,omitempty"`
	Region            string `json:"region,omitempty"`
	ProfileArn        string `json:"profileArn,omitempty"`
	HTTPStatus        int    `json:"httpStatus,omitempty"`
	UpstreamRequestID string `json:"upstreamRequestId,omitempty"`
	StartedAtMs       int64  `json:"startedAtMs"`
	DurationMs        int64  `json:"durationMs"`
	Outcome           string `json:"outcome"` // success | error
	ErrorType         string `json:"errorType,omitempty"`
	Error             string `json:"error,omitempty"`
	RetryAfter        string `json:"retryAfter,omitempty"`
}
```

Attach `Attempts []TraceAttempt` and `AttemptCount int` to `RequestLog` (both
`omitempty`). Emit exactly one `RequestLog` per client request — the attempts live
inside it — so row counts stop lying.

**Step 4:** rerun — PASS. **Step 5:** commit.

### Task 0.4: Log cache hits (fixes 1.4)

**Files:** `proxy/handler.go:1112-1122`.

Test: seed the response cache, issue the same request, assert one record exists with
`Outcome == "cache_hit"` and `DurationMs` present. Then add the `recordSuccessLog`
call with a `cacheHit: true` marker on the record.

### Task 0.5: Log rejected requests (fixes 1.5)

**Files:** `proxy/auth.go`, `proxy/rate_limiter.go`, `proxy/handler.go`.

Add `recordRejection(traceID, api, reason string, httpStatus int, apiKeyID string)`.
Call it from the auth failure and rate-limit paths. `Outcome` is `rejected`;
`ErrorType` is `auth` or `rate_limit`. Never log the offending key material — only the
key ID when one resolved, plus a masked suffix when it did not.

Test: two subtests asserting a 401 and a 429 each produce exactly one record with the
right `httpStatus` and no `ksk_` substring anywhere in the marshalled record.

### Task 0.6: Audit the clear action (fixes 1.10)

**Files:** `proxy/handler.go:4997`.

Call `h.appendAuditLog(AuditLog{Category: "logs", Action: "clear", Status: "success",
SafeDetails: map[string]string{"clearedCount": ...}})` before truncating. Test asserts
the audit log gains an entry.

---

## Phase 1 — Enrich the metadata record

### Task 1.1: Capture HTTP status and upstream request ID (fixes 1.6)

**Files:** `proxy/kiro.go`.

`CallKiroAPI` currently returns only `error`. Add an out-parameter struct so the
handler learns what happened upstream without parsing strings:

```go
// KiroCallDiagnostics reports per-endpoint upstream detail for one CallKiroAPI
// invocation. Populated even when the call ultimately fails.
type KiroCallDiagnostics struct {
	Attempts []TraceAttempt
}
```

Add `CallKiroAPIWithDiagnostics(account, payload, callback, *KiroCallDiagnostics) error`
and make the existing `CallKiroAPI` a thin wrapper passing `nil`, so the 8 existing
call sites keep compiling and can migrate one at a time. Inside the endpoint loop,
record `resp.StatusCode`, `resp.Header.Get("x-amzn-RequestId")`, the resolved host, and
the regionalised URL's region.

Test: `httptest` server returning 500 with an `x-amzn-RequestId` header; assert the
diagnostics carry status 500 and that request ID.

### Task 1.2: Split token counts and add stop reason

Add `InputTokens`, `OutputTokens`, `CacheReadTokens`, `StopReason`, `ResponseModel`,
`ToolCallCount` (all `omitempty`). Keep `Tokens` as the sum for backward compatibility
with the existing CSV export and the aggregations in `proxy/account_health.go:121` and
`proxy/usage_anomaly.go:70`.

### Task 1.3: Add TTFB

The stream handlers already have `reqStart`. Record the monotonic delta at the first
`OnText`/first SSE write into `TTFBMs`. Non-stream leaves it zero.

### Task 1.4: Add routing context

Add `Region`, `ProfileArn`, `UpstreamHost`, `API` (`claude|openai|responses`),
`Stream`, `HTTPStatus`, `Outcome`, `ApiKeyID`. This is what makes the post-IdC
multi-region behaviour debuggable.

---

## Phase 2 — Storage engine (fixes 1.8)

### Task 2.1: Append-only JSONL writer with a serialised single writer

**Files:** Create `proxy/trace_store.go`, `proxy/trace_store_test.go`.

One goroutine owns the file; producers hand records to a buffered channel. This kills
both the O(n) rewrite and the shared-temp-path race in one move.

```go
// traceStore appends trace records as JSONL. A single writer goroutine owns the
// file handle, so concurrent producers never interleave partial records and no
// shared temp path is rewritten per request.
type traceStore struct {
	dir     string
	ch      chan RequestLog
	done    chan struct{}
	dropped atomic.Int64
}
```

Non-blocking send: if the channel is full, increment `dropped` and move on. Logging
must never throttle serving. Expose `dropped` in the metrics summary so silent loss is
visible.

Tests: (a) 1000 concurrent appends produce 1000 valid JSON lines, no torn line;
(b) a full channel increments `dropped` instead of blocking; (c) file mode is `0600`.

### Task 2.2: Rotation and pruning

Rotate on UTC date change or `traceMaxIndexBytes`. Prune whole files older than
`traceRetentionHours`. Test with an injected clock.

### Task 2.3: Backfill-compatible loader

On boot, read the newest rotated files up to the ring capacity, and if
`data/request_logs.json` exists, load it too, then rename it to
`data/request_logs.json.migrated`. Test asserts a legacy file with today's 11-field
shape still populates the view.

---

## Phase 3 — Body capture (opt-in)

### Task 3.1: Config surface

**Files:** `config/config.go`.

```go
// TraceCaptureMode controls how much of each request is retained:
// "off", "meta" (default; no prompt or response text), "redacted", or "full".
TraceCaptureMode string `json:"traceCaptureMode,omitempty"`
// TraceCaptureAcknowledgeRisk must be true for "full" to take effect; otherwise
// "full" degrades to "redacted".
TraceCaptureAcknowledgeRisk bool `json:"traceCaptureAcknowledgeRisk,omitempty"`
TraceRetentionHours         int  `json:"traceRetentionHours,omitempty"`
TraceMaxBodyBytes           int  `json:"traceMaxBodyBytes,omitempty"`
```

Getters follow the existing `GetResponseCacheEnabled` pattern. Test the degrade rule
explicitly: `full` without the acknowledgement resolves to `redacted`.

### Task 3.2: Scrubber

**Files:** Create `proxy/trace_scrub.go` and `proxy/trace_scrub_test.go`.

`scrubTraceBody(raw []byte, mode string) []byte` — secret deny-list always, `redactPII`
when mode is `redacted`. Test that a body containing `ksk_live_abc123`, an
`Authorization: Bearer ...` line, and `"refreshToken":"..."` comes back with none of
those substrings, in **both** `redacted` and `full` modes.

### Task 3.3: Blob tier

`data/traces/bodies/YYYYMMDD/<traceID>.json.gz`, gzip, truncated to
`traceMaxBodyBytes` (default 256KB) with `bodyTruncated: true` set on the record.
`BodyRef` on the record is a relative path — never an absolute filesystem path in an
API response.

### Task 3.4: Fetch endpoint

`GET /admin/api/logs/{traceID}` returns the record plus decoded bodies. Behind the
existing admin auth, `Cache-Control: no-store`. 404 when capture was off. Test both.

---

## Phase 4 — Query API (fixes 1.9)

### Task 4.1: Structured filters and pagination

`GET /admin/api/logs` gains `from`, `to` (unix seconds), `limit` (default 100, max
1000), `cursor` (opaque, newest-first), `status`, `outcome`, `api`, `model`,
`accountId`, `apiKeyId`, `errorType`, `minDurationMs`, `q`. Honour `limit` — today it
is hardcoded to `0` at `proxy/handler.go:4961`.

Response adds `nextCursor` and `hasMore`. Keep the existing `logs`/`count`/
`persistedPath` keys so the current frontend keeps working mid-migration.

Tests: limit clamping, cursor stability across two pages with no duplicates or gaps,
and each filter in isolation.

### Task 4.2: Facets endpoint

`GET /admin/api/logs/facets` returns distinct models, accounts, error types, and
outcome counts for the active window, so the UI can populate dropdowns instead of
making the operator type substrings.

### Task 4.3: Export parity

CSV export must emit the new columns and stream row-by-row rather than materialising
everything. Add an `attempts` CSV variant (one row per attempt, joined by trace ID).

---

## Phase 5 — Frontend

### Task 5.1: Master/detail layout

**Files:** `web/index.html`, `web/app.js`, `web/styles.css`.

Rows become clickable, opening a right-hand drawer. Keep the table virtualised-lite:
render at most `limit` rows and paginate. Preserve the existing
`escapeHtml`/`escapeAttr` discipline on every interpolation — this is a
`innerHTML`-based renderer, so any unescaped field is an XSS vector.

### Task 5.2: Attempt waterfall

In the drawer, one row per `TraceAttempt`: sequence, account, region, profile suffix,
HTTP status, duration bar, error type badge. This is the view that makes 1.2 legible.

### Task 5.3: Body viewer

Two collapsible panes (request/response) with a copy button, only when `bodyRef` is
present. Show an explicit "capture mode: meta — bodies not stored" notice otherwise,
so absence is never mistaken for a bug.

### Task 5.4: Filter bar

Time range presets (15m/1h/24h/7d/custom), plus facet dropdowns, plus free text.
Persist selection in `localStorage`.

### Task 5.5: Locales

Add the new keys to **both** `web/locales/en.json` and `web/locales/zh.json`. The
symmetry check must stay green — it currently reports 778 leaves and the count must
match exactly between files.

---

## Phase 6 — Docs and settings UI

### Task 6.1: Settings panel

Capture-mode selector with an explicit warning on `redacted`/`full`, retention input,
and a live "bodies on disk: N files, M MB" readout.

### Task 6.2: Operator documentation

Create `docs/request-tracing.md`: the three tiers, the four capture modes, exactly
what is scrubbed, retention/pruning, disk-cost estimates, the API surface, and a
"how to debug a failing request in 60 seconds" walkthrough. Link it from `README.md`
and `README_CN.md` Operations sections. Add a `CHANGELOG.md` `[Unreleased]` entry.

---

## Validation gate (run after every phase)

```bash
cd /home/harry-riddle/dev/github.com/0xharryriddle/Kiro-Go
gofmt -l ./config ./proxy ./pool ./auth ./logger
go build ./...
go vet ./...
go test ./config/ ./pool/ ./auth/ ./proxy/
go test -race ./config/ ./pool/ ./auth/ ./proxy/
node --check web/app.js
python3 - <<'PY'
import json
def leaves(o, p=""):
    if isinstance(o, dict):
        return [k for kk, vv in o.items() for k in leaves(vv, p + kk + ".")]
    return [p[:-1]]
en = leaves(json.load(open("web/locales/en.json")))
zh = leaves(json.load(open("web/locales/zh.json")))
assert sorted(en) == sorted(zh), set(en) ^ set(zh)
print("locale symmetry OK:", len(en), "leaves")
PY
git diff --check
```

Two pre-existing `gofmt` flags (`proxy/usage_anomaly_test.go`,
`proxy/response_cache_test.go`) are untouched unless a task edits those files.

---

## Sequencing rationale

Phase 0 is independently shippable and fixes correctness bugs — requests that are
billed but unlogged, and failovers that vanish. Do not bundle it behind the storage
rewrite. Phase 2 must land before Phase 3, or body capture will amplify the existing
O(n)-per-request write. Phase 4 must land before Phase 5, or the UI will fetch
unbounded result sets.

## Risks

- **Privacy regression.** Body capture is the one feature here that can leak user
  prompts. Default `meta`, require an explicit acknowledgement for `full`, scrub in
  every mode, and test the scrubber adversarially.
- **Disk growth.** A busy proxy at `redacted` can produce gigabytes per day. Retention
  and byte caps are not optional polish; they ship with Phase 3.
- **Serving latency.** Trace writes must be non-blocking with a bounded queue and a
  visible `dropped` counter. Never let observability throttle the request path.
- **Signature churn.** `CallKiroAPI` has 8 call sites. The wrapper approach in Task
  1.1 keeps each migration a one-line change.
