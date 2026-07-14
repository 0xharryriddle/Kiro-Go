# Plan — Suppress upstream redaction-placeholder reasoning ("...")

Status: PLAN (implementation to follow in same change set)
Owner: 0xharryriddle
Scope: Kiro-Go proxy only. No hermes-agent edits.
Date: 2026-07-14

---

## 1. Symptom (verified live)

Calling `gpt-5.6-sol-thinking` returns a `reasoning_content` field whose value is
the literal 3-character string `...`:

```
prompt      effort     reasoning_content   content_len
trivial     default    (absent)            3
medium      default    "..."               153
hard        default    "..."               580
hard        minimal    "..."               573
hard        high       "..."               559
```

In the Hermes CLI this renders an empty "Reasoning: ..." panel that carries no
information.

## 2. Root cause (source-verified, not inferred)

- The model **does** reason (answer quality/length scales with difficulty; a
  `reasoningContentEvent` only fires for non-trivial prompts).
- GPT-5.x / o-series models **hide their raw chain-of-thought**. The Kiro
  upstream forwards a `reasoningContentEvent` whose `text` is a fixed redaction
  placeholder — always exactly `...`, independent of prompt or `reasoning_effort`
  (a real CoT would vary in length/content).
- Kiro-Go relays it verbatim: `proxy/kiro.go:534-540` (`reasoningContentEvent`
  case) calls `callback.OnText(normalized, true)` with whatever `text` arrives.
  The proxy does not fabricate the `...`; it passes upstream bytes through.

So nothing is broken — but surfacing a pure-placeholder reasoning string is
noise. The fix is to suppress reasoning content that carries **zero information**
(empty / whitespace / dots-or-ellipsis only), while passing **real** reasoning
(DeepSeek/Qwen thinking models emit genuine CoT) through unchanged.

## 3. Single chokepoint (why this is a small fix)

Every response path funnels reasoning through ONE function:

```
CallKiroAPI → parseEventStream(body, callback)   [proxy/kiro.go:433, 470]
                └─ case "reasoningContentEvent": callback.OnText(text, isThinking=true)
```

Both streaming and non-streaming, and all three APIs (OpenAI `/v1/chat/completions`,
Claude `/v1/messages`, OpenAI `/v1/responses`), consume this same callback:

- OpenAI stream: `handler.go:2164` emits `delta.reasoning_content`.
- OpenAI non-stream: `handler.go:2490-2510` → `KiroToOpenAIResponseWithReasoning`
  (`translator.go:2199`).
- Claude stream: `handler.go:1255-1310`.
- Claude non-stream: `handler.go:1896-1937` → `KiroToClaudeResponse`.
- Responses stream/non-stream: `responses_handler.go:154-187, 381-534`.

Because all six accumulate reasoning from `OnText(..., true)`, suppressing the
placeholder at `parseEventStream` fixes every route at once. No per-route edits
to the emit sites are required for the primary fix.

## 4. Design

### 4a. Classification helper (new, `proxy/kiro.go` or `translator.go`)
```go
// isPlaceholderReasoning reports whether a reasoning string carries no real
// content — it is empty, whitespace-only, or consists solely of dot/ellipsis
// runs (e.g. the "..." redaction marker upstream sends for hidden CoT). Real
// chain-of-thought always contains non-dot characters, so this never matches
// genuine reasoning.
func isPlaceholderReasoning(s string) bool {
    t := strings.TrimSpace(s)
    if t == "" {
        return true
    }
    for _, r := range t {
        // Allow '.', unicode ellipsis '…', and any whitespace; anything else
        // means real content is present.
        if r != '.' && r != '\u2026' && !unicode.IsSpace(r) {
            return false
        }
    }
    return true
}
```

### 4b. Suppress at the chokepoint (`proxy/kiro.go`, `reasoningContentEvent` case)
`normalizeChunk` sets `*previous = chunk`, so after the call
`lastReasoningContent` holds the cumulative reasoning so far. Gate the emit:

```go
case "reasoningContentEvent":
    if text, ok := event["text"].(string); ok && text != "" {
        normalized := normalizeChunk(text, &lastReasoningContent)
        if normalized != "" && callback.OnText != nil {
            // Suppress reasoning while the cumulative content is still a pure
            // redaction placeholder ("..."). The moment real reasoning text
            // appears, the cumulative is no longer placeholder-only and we emit
            // normally. Toggle via config (default: suppress).
            if suppressPlaceholderReasoning && isPlaceholderReasoning(lastReasoningContent) {
                // withhold: placeholder-only, no information
            } else {
                callback.OnText(normalized, true)
            }
        }
    }
```

`suppressPlaceholderReasoning` is read ONCE before the parse loop (not per event):
```go
suppressPlaceholderReasoning := config.GetThinkingConfig().SuppressPlaceholderReasoning
```

Behavioral guarantee: this can only ever withhold placeholder-only text. As soon
as one non-dot, non-space character arrives, the cumulative fails
`isPlaceholderReasoning` and every subsequent delta flows. Worst case for a real
model that literally opens its CoT with "..." is losing a couple leading dots —
acceptable and rare; real CoT does not open with dots-only.

### 4c. Defensive filter for the embedded-`<thinking>` path (non-stream)
`extractThinkingFromContent` (`translator.go:2172`) pulls reasoning out of
`<thinking>...</thinking>` embedded in content — a *different* source than
`reasoningContentEvent`. If a placeholder ever arrives that way, apply the same
helper at the non-stream finalization sites before attaching reasoning:
- OpenAI non-stream (`handler.go` ~2490): after computing `reasoningContent`, if
  `suppress && isPlaceholderReasoning(reasoningContent)` → set it to `""`.
- Claude non-stream (`handler.go` ~1897-1935): same guard on `rawThinkingContent`.
- Responses non-stream (`responses_handler.go` ~178, ~498): same guard.
These are cheap 1-line guards and make the fix robust regardless of which upstream
channel carries the placeholder.

### 4d. Config toggle (reversible, default = suppress)
`config/config.go`:
- Add field to the `Config` struct:
  `ShowPlaceholderReasoning bool `json:"showPlaceholderReasoning,omitempty"``
  Zero-value (absent) = false = "do not show" = suppress by default (standard
  on-by-default / opt-out pattern; omitempty is correct because the default is
  the zero value).
- Extend `ThinkingConfig` with `SuppressPlaceholderReasoning bool`.
- `GetThinkingConfig()`: set `SuppressPlaceholderReasoning: !cfg.ShowPlaceholderReasoning`.
- `UpdateThinkingConfig(...)`: add a `showPlaceholderReasoning bool` parameter and
  persist `cfg.ShowPlaceholderReasoning = showPlaceholderReasoning`. Update the
  one caller (`handler.go:5086`).

`proxy/handler.go`:
- `apiGetThinkingConfig` (5051): add `"showPlaceholderReasoning": cfg... `
  (report the raw show flag = `!SuppressPlaceholderReasoning`).
- `apiUpdateThinkingConfig` (5061): decode `ShowPlaceholderReasoning bool` and
  pass it through. No validation needed (bool).

### 4e. UI (`web/`) — optional operator control
- `web/index.html`: add a toggle in the existing Thinking settings block:
  "Show raw placeholder reasoning" (off by default).
- `web/app.js`: `loadThinkingConfig` reads `d.showPlaceholderReasoning` into the
  checkbox; `saveThinkingConfig` sends it.
- `web/locales/en.json` + `zh.json`: 2 keys
  (`settings.showPlaceholderReasoning`, `settings.showPlaceholderReasoningHint`).

## 5. Files touched (exhaustive)

| File | Change |
|------|--------|
| `proxy/kiro.go` | `isPlaceholderReasoning` helper + suppress in `reasoningContentEvent`; read toggle once (4a, 4b) |
| `proxy/handler.go` | non-stream defensive guards ×2 + thinking-config get/update wiring (4c, 4d) |
| `proxy/responses_handler.go` | non-stream defensive guard (4c) |
| `config/config.go` | `ShowPlaceholderReasoning` field, ThinkingConfig field, get/update (4d) |
| `proxy/*_test.go` | unit tests (§6) |
| `web/index.html`, `web/app.js`, `web/locales/en.json`, `web/locales/zh.json` | UI toggle (4e) |

No changes to routing, auth, streaming transport shape, or token accounting.

## 6. Tests

- `TestIsPlaceholderReasoning`: table — `""`, `" "`, `"."`, `"..."`, `"…"`,
  `".  ."` → true; `"real"`, `"...thinking"`, `"3.14"` → false.
- `TestParseEventStreamSuppressesPlaceholderReasoning`: feed a synthetic Kiro
  event stream whose only `reasoningContentEvent` text is `"..."`; assert the
  callback receives NO `isThinking=true` text when suppress=on, and DOES receive
  it when suppress=off.
- `TestParseEventStreamPassesRealReasoning`: reasoning text `"Let me think..."`
  → emitted verbatim regardless of toggle.
- Keep existing reasoning tests green (they use real text, unaffected).

## 7. Verification gate

1. `gofmt -l` clean on touched files → `go build ./...` → `go vet ./...`.
2. `go test ./config/ ./pool/ ./auth/ ./proxy/` → all pass.
3. `node --check web/app.js`; JSON valid; en/zh locale key parity.
4. Rebuild container; `/healthz` + `/readyz` 200.
5. **Live**: `gpt-5.6-sol-thinking` on a hard prompt →
   - default (suppress): `reasoning_content` is ABSENT/empty (no `...`), content intact.
   - after enabling "show raw": `reasoning_content` == `...` again (toggle proven reversible).
6. Confirm a real-CoT model (if reachable in fleet) still shows full reasoning.

## 8. Honest limits

- We cannot un-hide GPT-5.x's real chain-of-thought — the provider withholds it
  upstream; `...` is all Kiro forwards. This plan removes the noise, it does not
  recover hidden reasoning (impossible from the proxy).
- Streaming suppression is cumulative-prefix based: a real model opening its CoT
  with a dots-only run would lose those leading dots. Rare, information-free,
  acceptable.
