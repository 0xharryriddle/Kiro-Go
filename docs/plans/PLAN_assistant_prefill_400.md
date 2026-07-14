# Plan — Fix `assistant-prefill final message is not supported` HTTP 400

Status: PLAN ONLY (no code changes yet)
Owner: 0xharryriddle
Scope: Kiro-Go proxy only. **No hermes-agent edits.**
Date: 2026-07-11

---

## 1. Symptom (verified)

Main agent running `gpt-5.6-sol-thinking` via kiro-go (`http://localhost:8080/v1`)
aborts every turn once a goal loop / subagent review is in flight:

```
HTTP 400: assistant-prefill final message is not supported; last message must be user or tool
type: invalid_request_error   status: 400   elapsed: ~0.12s
Context: ~558 msgs, ~259,640 tokens
```

Non-retryable → the turn is killed and the large session is skipped for
persistence ("Skipping session persistence for large failed session").

## 2. Root cause (source-verified, not inferred)

The error string is emitted by **our own proxy**, not Hermes and not upstream Kiro:

- `proxy/handler.go:258` — `validateOpenAIRequestShape` returns the exact string
  when `lastRole == "assistant"` (OpenAI `/v1/chat/completions` route).
- `proxy/handler.go:150` — `validateClaudeRequestShape` has the twin guard for the
  Claude `/v1/messages` route (message ends `...must be user`).

Both live paths call the guard and hard-fail before translation:
- OpenAI: `handler.go:1968` in `handleOpenAIChat`.
- Claude: `handler.go:1042` in `handleClaudeMessagesInternal`.

### Why Hermes sends an assistant-tailed array (legitimate, expected)

Evidence from `~/.hermes/sessions/request_dump_*` (28 dumps triaged):

| model | n_msgs | last role | outcome |
|-------|--------|-----------|---------|
| gpt-5.6-sol-thinking | 269→283, 542→558 (steps of 2) | **assistant** | **PREFILL-400 (every one)** |
| claude-opus-4.8-thinking | 93–530 | tool / user | timeout / rate-limit (unrelated) |
| cx/gpt-5.6-sol (9Router) | 15, 49 | tool | InternalServerError (separate, see §7) |

Only the `-thinking` model on kiro-go ends on `assistant`, and every such request
is rejected. The `+2` message growth is the **goal loop** (`hermes_cli/goals.py`)
appending `user(continuation) → assistant(response)` pairs across iterations.

The assistant tail itself is produced by Hermes core's **thinking-prefill /
empty-response continuation** for reasoning models
(`agent/conversation_loop.py:4990-5007`): when a thinking model returns
reasoning-only or empty visible content, Hermes appends an
`_thinking_prefill` assistant message and re-sends the array to coax visible
output. One failing dump literally ends with assistant `content=''` — the
prefill/empty-sentinel shape. This is intentional Hermes behavior for reasoning
models and is **not** something we should (or are allowed to) change here.

The `-thinking` suffix is what activates it: `handler.go:1975`
`ParseModelAndThinking` sets `thinking=true` and injects `ThinkingModePrompt`;
Hermes then treats the model as a reasoner and enables the prefill path. The
plain (non-thinking) model and the opus-thinking model on the Claude route don't
hit this shape, which is why they don't 400.

### Why the guard is unnecessarily strict

The downstream translator **already tolerates a trailing assistant** — it just
never gets the chance because validation fails first:

- `OpenAIToKiro` (`translator.go:1126`): the `case "assistant"` at line 1177
  appends the assistant turn to `history` unconditionally (no `isLast` special
  case). With no trailing user, `currentContent` stays empty, so `finalContent`
  falls back to `minimalFallbackUserContent = "."` (`translator.go:1279-1287`,
  const at `translator.go:45`). Result: a **valid** Kiro `ConversationState`
  (assistant folded into History, synthetic `.` CurrentMessage).
- `ClaudeToKiro` mirrors this (`translator.go` ~207-302, same fallback ladder).

Kiro's upstream API has no assistant-prefill concept (CurrentMessage must be a
`UserInputMessage`), so folding-into-history + synthetic user is the only correct
representation — and the translator can already build it. **The 400 guard blocks
a payload the translator would handle.**

## 3. Fix strategy (kiro-go only)

Relax the two shape guards so a trailing assistant is accepted, and make the
translator's fold-into-history behavior explicit and safe rather than incidental.

### Change A — accept trailing assistant in shape validation
`proxy/handler.go`
- `validateOpenAIRequestShape` (line ~257): remove the hard
  `if lastRole == "assistant" { return ... }` rejection. Keep the
  `hasNonSystem` and `hasUserContext` guards (an assistant-tailed array still has
  prior user context, so `hasUserContext` stays satisfied — the array is not
  empty and has real user turns earlier).
- `validateClaudeRequestShape` (line ~149): same removal.

Rationale: shape validation should reject only what the translator cannot
represent. A trailing assistant IS representable (→ history + `.` current).

### Change B — make the translator's trailing-assistant handling explicit
`proxy/translator.go` (both `OpenAIToKiro` and `ClaudeToKiro`)
- Confirm/keep: trailing assistant → appended to `history`; `finalContent`
  falls back to `.` when no current user/tool/image.
- Add a small, greppable safeguard: when the **last** non-system message is an
  assistant prefill (empty or whitespace-only content, no tool calls), replace
  the synthetic `.` current message with an explicit continuation nudge constant
  (e.g. `assistantPrefillContinuation = "Continue."`) so Kiro produces a
  meaningful continuation instead of answering a bare `.`. When the trailing
  assistant carries real content (a replayed final turn), the `.` fallback is
  acceptable and preserves current behavior.
- Do **not** change how tool-tailed or user-tailed arrays are built.

### Change C — regression tests
`proxy/handler_test.go` (mirror existing patterns at lines 14-62)
- Flip `TestValidateOpenAIRequestShapeRejectsAssistantPrefill` /
  `TestValidateClaudeRequestShapeRejectsAssistantPrefill` to assert the trailing
  assistant is now **accepted** (rename to `...AllowsAssistantPrefill`), OR keep
  a guard test only for the genuinely-invalid shapes (empty array, no user
  context at all).
- Add `TestOpenAIToKiroFoldsTrailingAssistantIntoHistory`: given
  `[user, assistant]`, assert `payload.ConversationState.History` ends with the
  assistant turn and `CurrentMessage.UserInputMessage.Content` is the
  continuation nudge (Change B), not empty.
- Add the tool-tail happy-path assertion already covered by
  `TestValidateOpenAIRequestShapeAllowsToolResultFinalTurn` (keep as-is).

## 4. Files touched (exhaustive)

| File | Change |
|------|--------|
| `proxy/handler.go` | Remove 2 `lastRole == "assistant"` rejections (A) |
| `proxy/translator.go` | Explicit trailing-assistant→history + continuation-nudge const (B) |
| `proxy/handler_test.go` | Update/add tests (C) |

No changes to: auth, routing, streaming, kiro payload transport, or any
hermes-agent file.

## 5. Verification gate (must pass before claiming done)

1. `cd ~/dev/github.com/0xharryriddle/Kiro-Go && gofmt -l proxy/` → no output.
2. `go build ./...` → exit 0.
3. `go test ./proxy/ -run 'Validate|OpenAIToKiro|ClaudeToKiro|Prefill' -v` → PASS.
4. `go vet ./proxy/` → clean.
5. Rebuild binary, restart the :8080 listener (currently PID on `*:8080`).
6. **Live replay**: POST one captured failing payload (from a
   `request_dump_*_gpt-5.6-sol-thinking_*.json`) to `/v1/chat/completions` and
   confirm HTTP 200 with a non-empty completion (not 400). Use the dry-run
   diagnose route first (`apiReplayDiagnose`, handler.go:829) to confirm
   `validationError == ""`.
7. Re-run the original goal-loop workflow end-to-end; confirm no PREFILL-400 and
   the session persists.

## 6. Risks & honest limits

- **Why was the guard added?** Unknown from git log (recent commits are
  profile/auth). RISK: an earlier translator version produced broken payloads for
  trailing-assistant and the guard was a band-aid. MITIGATION: Change B makes the
  fold explicit + tested, so we remove the guard only after proving the translator
  output is valid (step 6 dry-run + live replay).
- **Semantic drift**: folding a *substantive* trailing assistant into history and
  generating against a nudge is not true "continue this assistant text" (Kiro
  can't do that). For the thinking-prefill case (empty tail) this is exactly
  right; for a replayed real-content tail it produces a fresh turn. Acceptable —
  it unblocks the loop and matches Kiro's capability ceiling.
- **Not the subagent 500s**: see §7.

## 7. Out of scope (separate issue, noted for honesty)

The `cx/gpt-5.6-sol` **subagents** run on **9Router-Proxy** (`localhost:20128`),
a different proxy. Their 2 failing dumps end on `tool` with
`InternalServerError` (HTTP 500) — an upstream/overload failure, **not** the
prefill 400 and **not** in Kiro-Go. Track separately; if subagent reviews keep
500ing, investigate 9Router health / context length (400k configured) rather
than this change.

## 8. Optional review step (per project convention)

AGENTS.md mandates `agent-debate-bus` for high-risk changes and the user prefers
a bounded advisor-executor soundness review before building. Recommended:
once the 9Router proxy is healthy, run a 2-reviewer debate on §3 (one soundness
reviewer on "does removing the guard ever send an invalid Kiro payload?", one
adversarial reviewer on the replayed-real-content-tail semantics). This can be
skipped if the user wants to proceed straight to implementation given the fix is
small and fully test-gated.

## 9. Execution order (when approved to build)

1. Change B (translator safeguard + const) + its unit tests → build/test green.
2. Change C (validation tests flipped) → build/test green.
3. Change A (remove guards) → build/test green (now that translator + tests prove safety).
4. Rebuild binary, restart :8080, run §5 steps 6-7 live replay.
5. Report diff stats + test counts + live replay HTTP status.
