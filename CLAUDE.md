# CLAUDE.md — Kiro-Go native Bedrock provider

Context for Claude Code when working on the Amazon Bedrock integration in this
Kiro-Go fork. Read this before touching Bedrock code.

## What this feature is

A native Bedrock account type (`AuthMethod: "bedrock"`) that calls the Bedrock
Runtime invoke endpoints directly with a static IAM access key (SigV4-signed),
instead of proxying through Kiro accounts or another Kiro-Go pool. For Claude models
Bedrock speaks the native Anthropic Messages wire format, so responses are re-emitted
to customers unchanged — the same transparent-passthrough shape the `custom_api`
forwarder uses.

## Repo conventions (follow these)

- **Zero third-party dependencies beyond `github.com/google/uuid`.** The codebase
  hand-rolls its own AWS event-stream parser rather than importing `aws-sdk-go`. Do
  NOT add `aws-sdk-go`/`aws-sdk-go-v2`. SigV4 is hand-rolled in `proxy/bedrock_sigv4.go`.
- Go 1.21 (`go.mod`). Standard library only for new code.
- Every non-trivial function carries a comment explaining *why*, not just *what* —
  match that density.
- Tests live beside code as `*_test.go` in the same package. The suite is
  comprehensive; keep it green.

## Where the pieces are

New files:
- `proxy/bedrock_sigv4.go` — `signSigV4(req, payload, creds, region, service, now)`
  and `awsURIEncodePath`. Cross-validated against botocore in the test.
- `proxy/bedrock_eventstream.go` — `readBedrockEventStream(body, onEvent)` peels AWS
  event-stream framing + the `{"bytes": base64(...)}` envelope to yield native
  Anthropic event JSON. Reuses `extractEventType` from `proxy/kiro.go`.
- `proxy/bedrock.go` — provider entrypoints `invokeBedrockStream` /
  `invokeBedrockNonStream`, model resolution (`resolveBedrockModelID`), body rewrite
  (`buildBedrockBody`), token extraction, and billing (`recordBedrockSuccess`).
- `proxy/admin_bedrock.go` — `POST /admin/add_bedrock_account`.

Edited files:
- `config/config.go` — `Account` gains `BedrockAccessKeyID`,
  `BedrockSecretAccessKey`, `BedrockSessionToken`, `BedrockModelMap`; plus
  `IsBedrock()`.
- `proxy/handler.go`:
  - dispatch branches in `handleClaudeStream` and `handleClaudeNonStream` (right
    after the `IsCustomApi()` branch),
  - a `IsBedrock()` early-return in `ensureValidToken` (static creds, no refresh),
  - the `/admin/add_bedrock_account` route,
  - guards excluding Bedrock accounts from Kiro-facing maintenance: background
    refresh, `refreshModelsCache`, `fetchAndCacheAccountModels`, the OpenAI stream /
    non-stream loops, `apiTestAccount`, `apiRefreshAccount`, `apiGetAccountModels`.

## Dispatch model (important)

Bedrock and custom_api accounts are transparent passthroughs. In the Claude handlers
the selection loop does: get account → `ensureValidToken` → if `IsCustomApi()` forward
→ **if `IsBedrock()` invoke Bedrock** → else translate to Kiro. A successful invoke
`return`s; a pre-stream failure sets `excluded[account.ID]`, calls
`handleAccountFailure`, and `continue`s to the next account. Preserve this contract:
Bedrock errors that happen *before any client bytes* must return an error so failover
works; errors *after* partial streaming are logged, not failed over.

## Billing / quota (already works — don't rebuild)

Customer keys already support token limits. `config.RecordApiKeyUsage(id, tokens,
credits)` adds tokens to `TokensUsed` and auto-disables the key when `TokenLimit` is
hit. The Bedrock path calls `h.recordSuccessForApiKey(apiKeyID, in, out, credits)`
which routes through that. Token-budgeted keys = set `TokenLimit`, leave
`CreditLimit` at 0.

## Invariants to never break

- No `aws-sdk-go`. SigV4 stays hand-rolled and stays cross-checked against botocore.
- Bedrock accounts must be excluded from every Kiro/AWS-SSO path (or they get 403'd
  and auto-banned). If you add a new Kiro-facing loop, add an `IsBedrock()` guard.
- The SigV4 known-answer test (`TestSigV4KnownAnswer`) must keep passing byte-for-byte.
- `go build ./... && go vet ./... && go test ./...` must be clean before you finish.

## Verified commands

```bash
go build ./...
go vet ./proxy/ ./config/
go test ./...                         # full suite
go test ./proxy/ -run Bedrock -v      # Bedrock tests only
```

## Known scope gaps (candidate follow-ups)

- ~~OpenAI-compatible surface is not wired for Bedrock~~ **CLOSED.**
  `/v1/chat/completions` serves Bedrock accounts today: `proxy/bedrock_openai.go`
  (794 lines) implements the OpenAI↔Anthropic bridge and is dispatched from
  `handleOpenAIStream` (`handler.go:2873`) and `handleOpenAINonStream` (`:3346`).
  The remaining gap is narrower: **`/v1/responses`** has no
  OpenAI-Responses→Anthropic translation, so it deliberately *skips* Bedrock
  accounts (`responses_handler.go:214`, `:443`) rather than 403-ing them. A
  Bedrock-only pool therefore cannot answer `/v1/responses`.
- ~~Model aliases in `defaultBedrockModelMap` are guesswork; ListFoundationModels
  would remove it~~ **CLOSED.** Auto-discovery ships in
  `proxy/bedrock_discovery.go` (415 lines): `discoverBedrockModels` (`:257`) calls
  `/foundation-models`, filters to ACTIVE on-demand text models (`:100`), merges
  inference profiles (`:138`), and caches per account with a TTL (`:180-215`).
  `resolveBedrockModelID` consults `BedrockModelMap` → `discoveredBedrockModelFor`
  (`bedrock.go:111`) → `defaultBedrockModelMap` (`:119`), so the static aliases are
  now only a last-resort fallback behind live discovery.
- Secrets are stored plaintext in config JSON (matches existing OAuth/Kiro-key
  storage). Encrypting secrets at rest would be a repo-wide improvement.
# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Kiro-Go is a reverse proxy that exposes Kiro accounts as OpenAI- and Anthropic-compatible API endpoints. It manages a pool of Kiro accounts, translates incoming Claude/OpenAI requests into Kiro's upstream AWS format, streams responses back, and serves a web admin panel. Single Go module, no external framework — stdlib `net/http` only, one dependency (`github.com/google/uuid`).

## Commands

```bash
go build -o kiro-go .          # build
go test ./...                  # run all tests
go test ./proxy/               # test one package
go test ./proxy/ -run TestName # run a single test
go vet ./...                   # vet
CONFIG_PATH=data/config.json ./kiro-go   # run (config auto-created if missing)
```

Docker: `docker-compose up -d` (mounts `./data` for persistence). CI builds the image via `.github/workflows/docker.yml`. The Dockerfile cross-compiles with `CGO_ENABLED=0`.

There is no linter config beyond `go vet`. Comments in the codebase mix English and Chinese — both are normal; match the surrounding file.

## Architecture

Request flow: **client → `proxy.Handler.ServeHTTP` (routing) → auth → translator → `pool` picks account → `CallKiroAPI` streams from AWS → translator maps events back → client.**

### Packages

- **`main.go`** — wires everything: load config, init logger, override password from `ADMIN_PASSWORD`, build `pool`, build `proxy.NewHandler()` (which starts background goroutines), start HTTP server. `WriteTimeout` is intentionally 0 so long SSE streams aren't cut off.
- **`config/`** — the single source of truth, a JSON file (`data/config.json`) guarded by one `sync.RWMutex`. Holds accounts, API keys, thinking config, prompt filters, proxy URL, regions, global stats. Getter/setter functions persist on write. Hot-path counter updates (`UpdateStats`, `UpdateAccountStats`, `RecordApiKeyUsage`) call `markDirtyLocked()` instead of writing inline; the proxy's background saver calls `FlushDirty()` every 30s to coalesce disk writes. On load it migrates legacy fields (single `ApiKey` → `ApiKeys[]`, per-account `allowOverage` → `OverageStatus`).
- **`pool/`** — `AccountPool` singleton (`GetPool()`). Weighted round-robin over enabled accounts; `Reload()` rebuilds the weighted slice from config and drops quota-blocked accounts. Tracks per-account cooldowns and error counts. `GetNextForModelExcluding(model, excluded)` is the main selection call — it skips cooled-down, soon-to-expire, quota-blocked, and wrong-model accounts. Classifies errors: `RecordError(id, isQuotaError)`, `IsAuthFailure`, `IsSuspensionError`.
- **`auth/`** — all the login/token flows: AWS Builder ID device flow (`builderid.go`), IAM Identity Center SSO (`iam_sso.go`), Kiro Hosted SSO incl. external IdP like Microsoft Entra (`kiro_sso.go`), SSO bearer token import (`sso_token.go`), local Kiro credential cache scan (`local_cache.go`), OIDC token refresh (`oidc.go` — `RefreshToken` is the entry point). `http_client.go` builds proxy-aware clients. `testhooks.go` exposes seams for tests.
- **`proxy/`** — the bulk of the logic:
  - `handler.go` — `Handler` struct, `ServeHTTP` routing, endpoint handlers, streaming logic for Claude (`handleClaudeStream`/`NonStream`) and OpenAI (`handleOpenAIStream`/`NonStream`), background refresh + stats saver goroutines, `ensureValidToken` (per-account mutex, double-checked refresh), and the admin API (`handleAdminAPI`, gated by `X-Admin-Password`).
  - `translator.go` — `ClaudeToKiro` / `OpenAIToKiro` request conversion, model-name normalization/aliasing, prompt sanitization (Claude Code prompt filter, env-noise stripping, custom regex rules), and payload truncation when the serialized body exceeds `config.GetMaxPayloadBytes()` (~2MB cap; AWS rejects >~2.15MB with `CONTENT_LENGTH_EXCEEDS_THRESHOLD`).
  - `kiro.go` — `CallKiroAPI`: the AWS Event Stream call. Tries `kiroEndpoints` in order (Kiro IDE / CodeWhisperer / AmazonQ) with fallback controlled by `config.GetEndpointFallback()`. Parses the binary event stream and drives `KiroStreamCallback` (`OnText`, `OnToolUse`, `OnComplete`, `OnCredits`, `OnContextUsage`).
  - `kiro_api.go` — REST calls: `ListAvailableModels`, `RefreshAccountInfo`, profile ARN resolution (regional — probes `kiroProfileRegions`).
  - `account_failover.go` — `handleAccountFailure` maps upstream error strings to actions (quota → cooldown, overage → refresh overage status, suspension/auth failure → disable account). `maxAccountRetryAttempts = 3`.
  - `responses_*.go` — OpenAI `/v1/responses` endpoint incl. stored-response history (auto-purged after 30 days).
  - `cache_tracker.go` — prompt-cache accounting for Claude usage reporting.
- **`web/`** — static admin panel (`index.html`, `app.js`, `styles.css`) and self-service usage portal (`portal.html` at `/check`). i18n locales in `web/locales`.
- **`logger/`** — leveled logger; level from `LOG_LEVEL` env or config, default `info`.

### Endpoints

API (require API key when any key is enabled): `/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`, `/v1/responses`, `/v1/models`, `/v1/stats`. Self-service (authenticate with the caller's own key): `/v1/key/info`, `/v1/key/logs`. Admin: `/admin` (page), `/admin/api/*` (password-gated). Public: `/health`, `/check`.

## Conventions and gotchas

- **Config is the persistence layer.** Any new persistent setting is a field on `Config` (or `Account`) plus a getter/setter in `config/config.go` that calls `Save()`. Bump `Version` in `config.go` and update `version.json` when releasing.
- **Never write config on the request hot path.** Use `markDirtyLocked()` + the background flush, following `UpdateStats`/`UpdateAccountStats`.
- **Account selection always goes through the pool**, never by iterating `config.GetAccounts()` directly in request handling. Respect cooldowns and the `excluded` set for retry loops.
- **Thinking mode**: triggered by a model-name suffix (default `-thinking`) or a Claude `thinking` config block. Output format is configurable per API (`claudeFormat`/`openaiFormat`: `thinking` / `think` / `reasoning_content`). The streaming handlers contain intricate state machines that reconcile two thinking sources (upstream reasoning events vs. inline `<thinking>` tags) — see `thinkingStreamSource`; don't let both emit.
- **Regions are resolved via fallback chains** (`Account.EffectiveAuthRegion` / `EffectiveApiRegion`): account-specific → account region → global → `us-east-1`. Auth region and API/data-plane region can differ; the profile ARN is authoritative for the data-plane region.
- **Error classification is string-matching** on upstream error messages (`account_failover.go`, `pool/account.go`). When touching error handling, keep these matchers in sync.
- **`.kiro/specs/`** holds design/requirements/tasks docs for in-progress feature work (e.g. external-IdP SSO hardening) — check there for context on larger changes.
- Tests live beside their code (`*_test.go`) and use the `auth/testhooks.go` seams to stub network calls.
