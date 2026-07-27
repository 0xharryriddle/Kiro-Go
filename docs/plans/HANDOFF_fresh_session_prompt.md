# Kiro-Go — fresh-session handoff prompt

Paste this whole file as the opening message of the new session. Every fact below
was verified by command output at handoff time (2026-07-27, HEAD `5c748be`).
Where something is unverified or unknown, it says so — do not upgrade those to
facts without checking.

---

## 1. Your task

Continue the Kiro-Go audit-and-harden effort. It is not "finish a feature"; it is
**find real defects, prove them, fix them, verify, deploy**. The previous session
closed 62 defects across seven rounds. There is no deadline and no fixed list —
work the highest-risk unreviewed surface, then the next.

Repo: `/home/harry-riddle/dev/github.com/0xharryriddle/Kiro-Go`
Branch: `harry` (tracks `origin/harry`)

---

## 2. Verified state at handoff

| Fact | Value |
|---|---|
| HEAD | `5c748be` |
| Remote | `origin/harry` identical (0 ahead / 0 behind) |
| Working tree | clean |
| Tests | 883 pass across `config` `pool` `auth` `proxy` |
| `-race` | clean, 0 data races |
| `go vet` / `gofmt` | clean tree-wide |
| Live container | healthy, version **1.1.5** |
| Deployed image built from | `16a71ad` (HEAD is one tests-only commit ahead, so no redeploy is pending) |

Recent commits (newest first):

```
5c748be test(tokens): pin the wire-vs-report estimator safety invariant
16a71ad fix(admin): bound request bodies on the admin-bot API surface
177f975 docs(checkpoint): record the unauthenticated surface as open by design
5b6fa90 fix(stream): stop labelling OpenAI client faults as server errors
5172d4b fix(stream): close three defects in the failure-frame observer
3242fff fix(stream): correct three Claude error-type mappings against the official enum
cf8ed84 test(stream): pin that exception detection cannot break a working stream
7bfbbb0 feat(stream): classify Claude mid-stream errors and observe upstream exception frames
```

**Read `docs/plans/CHECKPOINT_audit_and_merge_state.md` first.** It is the
authoritative record: all 62 defects, the rejected claims (so they are not
re-litigated), and every deliberate non-decision with its reasoning.

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
  especially your own. 15 of the 62 defects were regressions introduced by
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

**Rollback:** the pre-merge image is GONE (garbage-collected). Real rollback is
`git checkout 99dda52 && docker compose up -d --build`, or the extracted binary at
`/tmp/kirogo_rollback_binary` if it still exists.

---

## 6. Open work — highest risk first

### 6a. Unreviewed files with no test coverage

| File | Lines | Why it matters |
|---|---|---|
| `proxy/websearch_loop.go` | 600 | agentic loop; touched by recent fixes but never audited whole |
| `proxy/bedrock.go` | 502 | the ONLY path where prompt caching actually works |
| `proxy/request_trace_recorder.go` | 483 | decides what reaches disk; PII/redaction relevance |
| `auth/sso_token.go` | 338 | credential handling |

Method that worked: find the largest non-test file lacking a sibling `_test.go`,
read it, look for (a) unbounded reads/allocations, (b) inconsistency with a
sibling that already does it right, (c) invariants nothing pins.

### 6b. Blocked pending observation — do not force

**Failure-frame promotion.** `parseEventStream` now OBSERVES AWS exception frames
(`:message-type: exception|error`) and logs them, but never treats them as fatal.
Promoting specific `:exception-type` values to abort a stream needs a **real
observed frame**. The trace corpus cannot supply one (`noteResponseText` stores
assembled text, so frame headers are destroyed before capture). Check for
accumulated evidence:

```bash
docker logs kiro-go-kiro-go-1 2>&1 | grep -c "upstream failure frame"
```

Zero at handoff. Acting on the AWS spec alone risks killing live streams on the
hot path — worse than the imprecision it fixes.

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
