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
