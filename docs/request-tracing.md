# Request tracing

Kiro-Go records one **trace** per client request: what was asked, which upstream
account served it, every attempt it took to get there, and optionally the request
and response bodies.

This document covers the storage tiers, the four capture modes, exactly what is
scrubbed, retention, disk cost, the API surface, and a 60-second debugging
walkthrough.

---

## 1. Why traces exist

Before tracing, the Logs view had structural blind spots:

- A request that failed over across three accounts emitted **one** record
  carrying only the *final* error. The quota error that caused the reroute — the
  evidence that explains the reroute — was discarded.
- Cache hits updated the usage counters but wrote **no** log record, so
  `logCount` could never be reconciled against `totalRequests`.
- Requests rejected before reaching a handler (bad API key, rate limit) produced
  no record at all. A tenant hammering an invalid key was invisible.
- The upstream HTTP status code survived only as text inside an error string, and
  any upstream correlation ID was thrown away.

A trace fixes all four: exactly **one record per client request**, with each
upstream attempt nested inside it.

---

## 2. Storage tiers

| Tier | Contents | Default | Location |
|---|---|---|---|
| Index | one metadata line per request | always on | `data/traces/index-YYYYMMDD.jsonl` |
| Attempts | per-account/per-endpoint detail | always on | embedded in the index line |
| Bodies | request/response payloads | **off** | `data/traces/bodies/YYYYMMDD/<traceID>.json.gz` |

The index is **append-only JSONL**, one record per line, written by a single
goroutine. Producers hand records to a bounded queue and never block: if the
queue is full the record is dropped and counted, because observability must never
throttle request serving. The drop counter is surfaced in the Logs pager and in
the Settings storage readout, so silent loss is visible rather than mysterious.

Files rotate on UTC date change. The live admin view is backed by an in-memory
ring of the most recent 500 records; longer history comes off disk.

### Upgrading from the pre-trace format

On first boot, an existing `data/request_logs.json` is imported into the JSONL
store and then renamed to `data/request_logs.json.migrated`. The original file is
never deleted. Migrated rows are written into today's index, so history survives
the upgrade rather than appearing to vanish.

---

## 3. Capture modes

Set via the admin UI (Settings -> Request tracing), `traceCaptureMode` in
`data/config.json`, or the `TRACE_CAPTURE_MODE` environment variable.

| Mode | Metadata | Attempts | Bodies |
|---|---|---|---|
| `off` | no | no | no |
| `meta` (**default**) | yes | yes | **no** |
| `redacted` | yes | yes | yes, PII-redacted |
| `full` | yes | yes | yes, verbatim text |

**`meta` is the default and stores no prompt or response text whatsoever.**
Upgrading Kiro-Go therefore changes nothing about what is retained.

`full` additionally requires `traceCaptureAcknowledgeRisk: true`. Without that
flag, `full` **degrades to `redacted`** — verbatim prompt retention is always a
deliberate two-step decision, never a single typo. An unrecognised or malformed
mode value fails safe to `meta`.

The `GET /admin/api/settings` response reports the *resolved* mode, so a `full`
setting without the acknowledgement is reported as the `redacted` it actually
behaves as.

This mirrors the OpenTelemetry GenAI semantic conventions, which classify
`gen_ai.input.messages` and `gen_ai.output.messages` as `Opt-In` precisely
because they are "likely to contain sensitive information including user/PII
data".

### Environment overrides

```bash
TRACE_CAPTURE_MODE=redacted      # off | meta | redacted | full
TRACE_CAPTURE_ACK_RISK=true      # required for full to take effect
```

Useful for a short debugging window without editing `config.json`.

---

## 4. What is scrubbed

Credential scrubbing is **unconditional in every mode that captures anything**,
including `full`. `full` governs how much *prompt text* is retained, never
whether *secrets* are retained.

Always removed, from bodies and from error strings:

- Kiro-issued API keys (`ksk_...`) — the `ksk_` prefix is kept so an operator can
  still tell which kind of credential was involved
- `Bearer <token>` in headers or quoted error text
- JSON credential fields: `accessToken`, `refreshToken`, `idToken`,
  `clientSecret`, `apiKey`, `x-api-key`, `password`, `authorization`, `cookie`,
  `secret` — in both header form (`x-api-key: value`) and JSON form
  (`"x-api-key": "value"`)
- AWS SSO/OIDC-style opaque credentials echoed in upstream errors

In `redacted` mode, bodies additionally pass through the same `redactPII` used
for outbound prompt filtering: email addresses, credit-card-like numbers, US
SSNs, and IPv4 addresses become typed placeholders such as `[REDACTED_EMAIL]`.

Rejection records never carry the offending credential. When authentication fails
before an API key resolves, the record stores a suffix-only fingerprint
(`invalid:****abcd`) rather than the key, and never the first characters — for a
*rejected* key those are real credential prefix an operator does not need.

Profile ARNs are reduced to their trailing identifier, so a trace shows which
profile served a request without echoing the embedded AWS account ID into every
row.

File permissions are `0600` under a `0700` directory, because trace files can
contain prompt text once body capture is enabled.

---

## 5. Retention and disk cost

| Setting | Default | Meaning |
|---|---|---|
| `traceRetentionHours` | 168 (7 days) | age at which whole rotated files are pruned |
| `traceMaxBodyBytes` | 262144 (256 KB) | per-body cap; larger bodies are truncated and flagged |

Pruning runs at startup and hourly, and removes **whole files or whole day
directories** — never rewriting live data. Bodies prune on the same clock as the
index: retaining prompt payloads longer than the metadata referencing them would
leave orphaned sensitive data with nothing pointing at it.

Rough cost per 1,000 requests:

| Mode | Approx. disk |
|---|---|
| `meta` | ~400 KB (measured ~403 bytes/record on a live single-attempt workload; longer failover chains cost more because each attempt is embedded) |
| `redacted` / `full` | highly workload-dependent; a 20 KB prompt+response pair gzips to a few KB, so budget single-digit MB per 1,000 requests |

A busy proxy at `redacted` can produce gigabytes per day. Retention and the byte
cap are not optional polish. Check the live figures in Settings -> Request
tracing, which reports index files, body files, bytes for each, records written,
and records dropped.

---

## 6. API surface

All endpoints sit behind the existing admin auth (`X-Admin-Password` header or
the `admin_password` cookie).

### List traces

```
GET /admin/api/logs
```

| Parameter | Meaning |
|---|---|
| `status` | `success` \| `error` \| `all` |
| `outcome` | `success` \| `error` \| `cache_hit` \| `rejected` |
| `api` | `claude` \| `openai` \| `responses` |
| `model`, `accountId`, `apiKeyId`, `errorType` | exact match |
| `from`, `to` | Unix seconds |
| `minDurationMs` | slow-request floor |
| `q` | free-text across metadata *and* nested attempts |
| `limit` | default 100, clamped to 1000 |
| `cursor` | opaque, newest-first |
| `format` | `json` \| `csv` \| `attempts-csv` |

Response carries `logs`, `count`, `persistedPath` (unchanged for backward
compatibility) plus `total`, `limit`, `nextCursor`, `hasMore`, and `dropped`.

The cursor embeds both timestamp and trace ID. Timestamps have one-second
resolution and collide freely under load, so a time-only cursor would skip or
repeat rows within the same second. A stale cursor degrades to "first page"
rather than erroring.

### Trace detail

```
GET /admin/api/logs/{traceID}
```

Returns the full record including every attempt, plus decoded bodies when they
were captured. Sets `Cache-Control: no-store` because the response can carry
prompt text. Returns 404 for an unknown ID. When bodies were not captured, the
response says so explicitly via `bodiesStored: false` and a `note` naming the
active capture mode — absence is never presented as a malfunction.

`bodyRef` is always a path relative to the trace directory; absolute filesystem
paths are never returned.

### Facets

```
GET /admin/api/logs/facets
```

Distinct models, APIs, accounts, error types, and outcome counts in the retained
window, sorted so UI dropdowns do not reshuffle between polls.

### Storage

```
GET /admin/api/logs/storage
```

File counts, byte totals, records written, records dropped.

### Clear

```
DELETE /admin/api/logs
```

Clears the in-memory ring and removes the legacy file. **Clearing is audited** —
it destroys evidence, so the action itself leaves an audit-log entry recording
how many records were cleared.

---

## 7. Field reference

Field names are camelCase transliterations of OpenTelemetry GenAI semantic
convention attributes where one exists, so a future OTel/Langfuse exporter is
mechanical rather than a re-modelling exercise.

| Trace field | OTel GenAI attribute |
|---|---|
| `model` | `gen_ai.request.model` |
| `responseModel` | `gen_ai.response.model` |
| `inputTokens` | `gen_ai.usage.input_tokens` |
| `outputTokens` | `gen_ai.usage.output_tokens` |
| `stopReason` | `gen_ai.response.finish_reasons` |
| `ttfbMs` | `gen_ai.server.time_to_first_token` |
| `duration` | `gen_ai.server.request.duration` |
| `stream` | `gen_ai.request.stream` |
| `errorType` | `error.type` |
| `upstreamHost` | `server.address` |

`status` remains `success`/`error` only, because account-health and
usage-anomaly aggregation switch on it. `outcome` carries the finer detail
(`success`, `error`, `cache_hit`, `rejected`).

Each entry in `attempts[]` carries `seq`, `accountId`, `accountEmail`,
`upstreamEndpoint`, `upstreamHost`, `region`, `profileArn` (suffix only),
`httpStatus`, `upstreamRequestId`, `startedAtMs`, `durationMs`, `outcome`,
`errorType`, `error`, and `retryAfter`.

`region` on an attempt is the region the request was **actually dispatched to**,
derived from the resolved host. This can legitimately differ from an account's
auth region — an IAM Identity Center account's portal region is routinely in a
different region from the profile serving its traffic, and that mismatch is
exactly what a trace needs to make visible.

---

## 8. Debug a failing request in 60 seconds

1. **Open Logs.** Set the time range to 15m and Status to `error`.

2. **Scan the Tries column.** Any value above 1 is highlighted: the request was
   rerouted. That alone tells you it was not a single clean failure.

3. **Click the row.** The drawer opens with the trace.

4. **Read the attempt waterfall.** One line per upstream attempt: sequence,
   account, region, profile suffix, HTTP status, a duration bar, and an error
   badge. This is where "account A hit quota, account B returned 500, account C
   served it" becomes legible.

5. **Check `upstreamRequestId`** on the failing attempt. That is the correlation
   ID to quote in a Kiro/AWS support escalation.

6. **Compare `ttfbMs` against `duration`.** A large gap on a streaming request
   means the model was slow to first token, not that the network stalled.

7. **If you need the payload**, set capture to `redacted` in Settings, reproduce
   once, then re-open the trace — the drawer will show request and response
   panes. Set it back to `meta` afterwards.

Common signatures:

| What you see | What it means |
|---|---|
| `errorType: quota` on attempt 1, success on attempt 2 | normal failover; the pool is doing its job |
| Every attempt `errorType: quota` | the whole pool is exhausted for that model |
| `errorType: auth` on one account only | that account's token needs re-auth |
| `errorType: profile` | profile ARN resolution or a region pin mismatch |
| `outcome: rejected`, `errorType: rate_limit` | your own API key's RPM/TPM limit, not upstream |
| `outcome: rejected`, `errorType: auth` | client is sending a bad or disabled API key |
| `outcome: cache_hit` | served from the response cache; still billed to the key |
| `dropped` climbing in the pager | trace queue saturating under load; records are being lost |

---

## 9. Privacy checklist before enabling body capture

- [ ] Do you actually need payloads, or would `meta` plus the attempt waterfall
      answer the question?
- [ ] Is `traceRetentionHours` short enough for the debugging window you need?
- [ ] Is `traceMaxBodyBytes` low enough for your disk?
- [ ] Are you prepared to set the mode back to `meta` afterwards?
- [ ] Does anyone with admin-panel access have a legitimate need to read user
      prompts?

Enabling `redacted` or `full` writes an audit-log entry recording the mode,
acknowledgement, retention, and body cap. That trail is deliberate.
