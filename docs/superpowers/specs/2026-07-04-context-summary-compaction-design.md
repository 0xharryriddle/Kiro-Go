# Context Compaction via LLM Summary — Design

## Problem

When a conversation exceeds the model's input-token window, the proxy today
silently drops the oldest history turns (`truncatePayloadToLimit` in
[translator.go](../../../proxy/translator.go)). Dropped turns are gone: facts,
decisions, and open tasks from early in the session vanish. Users asked for
**summarization instead of hard truncation** — replace the dropped span with an
LLM-generated summary so earlier context survives in compressed form.

## Goal

When an over-budget payload would otherwise be truncated, generate a concise
LLM summary of the turns being cut and insert it in their place. Keep the most
recent turns verbatim. Degrade to today's hard truncation on any failure.

Non-goals: proactive/early compaction below the window; changing the byte cap;
new API endpoints; summarizing the current message.

## Trigger

Unchanged from today's detection: after `ClaudeToKiro` / `OpenAIToKiro` build
the payload, compaction fires when
`payloadInputTokenSize(payload) > maxInputTokensForModel(payload, model)`
**and** the feature toggle `EnableContextSummary` is on.

The toggle defaults **off** — summarization costs upstream credits and adds
latency before first token. Read per-request (like `GetMaxPayloadBytes`) so it
can change without restart.

## Where it runs

LLM summarization needs an upstream call, which needs an account and
`CallKiroAPI`. Today's truncation lives in the pure `ClaudeToKiro` (no account /
HTTP access). Compaction therefore moves **up into the handler layer**:

- New step in `handleClaudeMessagesInternal` and the OpenAI equivalent, after
  the payload is built and before dispatching to stream / non-stream.
- Picks a summarizer account via `h.pool.GetNextForModelExcluding(model, nil)`
  and reuses `CallKiroAPI` (non-stream collection of the text) for the summary.
- The pure translator keeps its existing `truncatePayloadToLimit` call as the
  final safety net; the new handler step runs first and, on success, leaves a
  payload that no longer needs hard truncation (but truncation still runs
  defensively).

## Components

### 1. `foldSummary` — the core primitive

```
foldSummary(prev string, entries []KiroHistoryMessage, model string,
            account *config.Account) (string, error)
```

Accumulates the flattened text of `entries` into a chunk. When
`prev + chunk` approaches ~50% of the model window, it fires **one** upstream
LLM call:

> "Summarize the following conversation history concisely. Preserve facts,
> decisions, code, file paths, and open tasks. Prior summary followed by new
> turns: `<prev>` `<chunk>`"

The result becomes the new `prev`; the loop continues until all entries are
consumed. Because each sub-request is size-capped at ~50% of the window, the
summary call itself can never overflow — this handles both:

- **Cold start**: history far larger than the window → multiple chunked folds.
- **Incremental**: tiny new delta folded into a cached prior summary → one call.

The summary call builds a minimal `KiroPayload` (single user message, no tools,
bounded `MaxTokens` for the reply, e.g. ~2000 tokens) and collects the streamed
text via a `KiroStreamCallback` that appends `OnText`.

### 2. Cut point

Reuse the suffix-fitting loop from `truncatePayloadToLimit`, but reserve room
for one summary entry (capped, ~2000 tokens) in the retained tail. Result:

- `cut` = index splitting old (to summarize) from recent (kept verbatim).
- `old = conversation[:cut]`, `recent = conversation[cut:]`
  (`recent` always ≥ `minRecentHistoryTurns`).
- If `cut == 0` (nothing old enough to summarize), skip to fallback.

Priming pair (if present) is excluded from `conversation` and always kept at the
front, exactly as today.

### 3. Cache

In-memory, keyed by `ConversationID` (already stable across turns via
`buildConversationID`). Same lock + TTL-prune pattern as `promptCacheTracker`.

```
type summaryCacheEntry struct {
    prefixHash   [32]byte  // hash of conversation[:coveredCount]
    coveredCount int       // how many leading conversation entries the summary covers
    summary      string
    expiresAt    time.Time
}
```

On compaction:

- **Incremental** — if `cached.coveredCount <= cut` and
  `hash(conversation[:coveredCount]) == cached.prefixHash`:
  `summary = foldSummary(cached.summary, conversation[cached.coveredCount:cut])`.
- **Cold** — otherwise:
  `summary = foldSummary("", conversation[:cut])`.

Store `{hash(conversation[:cut]), cut, summary}` after either path.

The prefix hash guards against cache reuse when early history changed (e.g. the
client edited an old turn), which would make an incremental fold incorrect.

### 4. Rebuild

```
history = priming
        + [summary as one KiroUserInputMessage]
        + dropLeadingAssistant(conversation[cut:])
```

The summary entry is a `KiroUserInputMessage` (Origin `AI_EDITOR`, model =
current) prefixed with a marker note so the model knows it is a compaction of
elided history (analogous to today's `truncationPlaceholder`).

After rebuild, `truncatePayloadToLimit` runs as the final safety net: if summary
+ recent turns are *still* over budget, it hard-trims the remainder.

## Data flow

```
build payload (ClaudeToKiro/OpenAIToKiro)
        │
        ▼
over budget && EnableContextSummary? ──no──► existing truncatePayloadToLimit ──► CallKiroAPI
        │yes
        ▼
compute cut point (reserve summary room)
        │
   cut == 0? ──yes──► fallback: truncatePayloadToLimit ──► CallKiroAPI
        │no
        ▼
cache lookup by ConversationID
        │
   incremental vs cold ──► foldSummary(...)
        │
   err? ──yes──► log + fallback: truncatePayloadToLimit ──► CallKiroAPI
        │no
        ▼
store cache entry
        │
        ▼
rebuild history: priming + summary + recent
        │
        ▼
truncatePayloadToLimit (safety net) ──► CallKiroAPI (main request)
```

## Error handling

Any `foldSummary` error (network, quota, non-200, empty result) → log at warn,
fall through to today's `truncatePayloadToLimit`. The main request always
proceeds; only compaction quality degrades. No error surfaces to the client.

## Config

New field on the config struct:

```
EnableContextSummary bool `json:"enableContextSummary,omitempty"`  // default false
```

Accessor `GetEnableContextSummary()` mirroring `GetMaxPayloadBytes()`. Optional
follow-up (not required for v1): expose in admin UI. v1 ships the backend knob.

## Testing

- `foldSummary` chunking: history larger than one fold window produces multiple
  bounded sub-calls (mock the upstream call to assert each sub-payload is
  size-capped and that folds chain). Verify final summary is non-empty.
- Cut point reserves summary room: retained tail ≥ `minRecentHistoryTurns` and
  the reserved slot is accounted for.
- Cache incremental path: second compaction on an extended conversation folds
  only the delta (mock asserts the prior summary is passed as `prev` and only
  new entries are summarized).
- Cache invalidation: mutating an early entry breaks the prefix hash → cold
  fold, not incremental.
- Fallback: a `foldSummary` error leaves the payload trimmed by
  `truncatePayloadToLimit` (placeholder present, current message preserved) —
  reuse the existing truncation assertions.
- Toggle off: over-budget payload takes the pure-truncation path, no upstream
  summary call is made.
- Rebuild shape: history = priming + one summary user message + verbatim recent
  turns; no leading assistant after the summary.

## Files touched

- [config/config.go](../../../config/config.go) — `EnableContextSummary` field +
  `GetEnableContextSummary()`.
- [proxy/translator.go](../../../proxy/translator.go) — expose cut-point helper
  (refactor the suffix loop so both truncation and compaction share it); summary
  entry builder + marker constant.
- New `proxy/context_summary.go` — `foldSummary`, the summary cache, and the
  handler-level `compactPayload(h, payload, model)` entry point.
- [proxy/handler.go](../../../proxy/handler.go) — call `compactPayload` in the
  Claude and OpenAI internal handlers before dispatch.
- New `proxy/context_summary_test.go` — the tests above.
