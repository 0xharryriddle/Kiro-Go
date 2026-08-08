# Kiro-Go (Enhanced Fork)

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Turn Kiro accounts into an OpenAI- and Anthropic-compatible API service.

[English](README.md) | [中文](README_CN.md) | [Tiếng Việt](README_VI.md)

> This is an **enhanced fork** of the upstream Kiro-Go. It keeps full compatibility with the original endpoints and admin panel, and adds per-key rate limiting, a shared proxy pool with failover, model overrides, DoS protection, and a self-service usage dashboard. See [What's new vs. the original](#whats-new-vs-the-original).

## What it does

Kiro-Go is a reverse proxy. It manages a pool of Kiro accounts, translates incoming Claude/OpenAI requests into Kiro's upstream AWS format, streams responses back, and serves a web admin panel. It's a single Go module built on the standard library `net/http` — one dependency (`github.com/google/uuid`).

Request flow: **client → routing → auth → translator → pool picks account → stream from AWS → translate events back → client.**

## Features

- Anthropic `/v1/messages` & OpenAI `/v1/chat/completions`
- Multi-account pool with round-robin load balancing
- Auto token refresh, SSE streaming, Web admin panel
- Multiple auth: AWS Builder ID, IAM Identity Center (Enterprise SSO), Microsoft Enterprise SSO, SSO Token, local cache, credentials JSON, Kiro API Key
- Usage tracking, account import/export, i18n (CN / EN)
- Support configuring outbound proxy (SOCKS5 / HTTP)
- Anthropic `/v1/messages` and OpenAI `/v1/chat/completions` (+ `/v1/responses`, `/v1/models`, `/v1/stats`)
- Multi-account pool with weighted round-robin load balancing and automatic failover
- Auto token refresh, SSE streaming, web admin panel
- Multiple auth methods: AWS Builder ID, IAM Identity Center (Enterprise SSO), Kiro Hosted SSO (incl. Microsoft Entra / external IdP), SSO token import, local Kiro credential cache, credentials JSON
- Usage tracking, account import/export, i18n (EN / 中文 / Tiếng Việt)
- Outbound proxy support (SOCKS5 / HTTP)
- Thinking mode (per-suffix or Claude `thinking` config)

## What's new vs. the original

Compared to the upstream Kiro-Go, this fork adds:

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
| Area | Feature |
|------|---------|
| **Per-key limits** | RPM limit (token-bucket delay, not hard error), concurrent-IP cap, IP allowlist (IP/CIDR), TPM display, per-key token/credit quota |
| **Friendly limit notice** | A configurable in-chat reply shown when a key is blocked (disabled / expired / over-limit / IP-denied) instead of a raw 401/429 |
| **Model overrides** | **Force Model** (global override for every request) and **per-key Model** — remap client-requested model names to a real upstream model without touching the client |
| **Identity model** | Tell the assistant to self-identify as a given model name without changing which upstream model actually serves the request |
| **Bound accounts** | Pin an API key to a fixed set of accounts (empty = shared pool) |
| **Lifetime counters** | Grand-total request/token/credit counters that survive a routine per-cycle "Reset Usage", plus "Reset All" |
| **Bulk key ops** | Bulk create / delete API keys, editable key values, JSON export (preserves External IdP metadata so Entra accounts re-import correctly) |
| **Shared proxy pool** | A pool of outbound proxies with persisted health and proxy-level failover — a dead proxy is skipped and retried after a cooldown, across restarts. Plus a **Require-proxy** safety toggle and per-request routing logs |
| **DoS protection** | App-layer guard (`proxy/dos_guard.go`): body-size cap, global concurrency limit, per-IP RPM reject, per-key inflight cap, trusted-proxy IP resolution — all tunable via env vars |
| **Admin hardening** | Brute-force throttle and session handling for the admin API |
| **Self-service usage** | A `/usage` dashboard where a key owner can view their own usage/logs, plus the `/check` portal |
| **Realtime UI** | The admin panel auto-refreshes lists (accounts, keys) without a manual reload |

## Quick start

### Windows (local, one-click)

```bat
run.bat
```

`run.bat` stops any previous instance, finds a free port, builds, and runs. Requires [Go](https://go.dev/dl/) installed and an existing `data/config.json`.

### Build from source

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/zsecducna/kiro-go:latest
```

### Build from Source

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go

# Run on a different port (flag > PORT env > config.json):
./kiro-go -port 9090
# or: PORT=9090 ./kiro-go
```

Config is auto-created at `data/config.json` if missing. Open <http://localhost:8080/admin>.

### Docker Compose (recommended for servers)

```bash
docker compose up -d --build
```

Use `--build` whenever the code changes, otherwise compose reuses a stale cached image. See [DEPLOYMENT.md](DEPLOYMENT.md) for the full Docker guide, including the SSO loopback-port mechanism.

The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8080/admin`, log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-key" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

### Add a Kiro API Key account

In the admin panel, choose **API Key** when adding an account and paste `ksk_...` (or `ksk_...|region`).

You can also import via the credentials API:

```bash
curl -X POST http://localhost:8080/admin/api/auth/credentials \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session>" \
  -d '{"kiroApiKey":"ksk_your_key|us-east-1","authMethod":"api_key","nickname":"cli-key"}'
```

API Key accounts call the Kiro CLI runtime (`https://runtime.{region}.kiro.dev/`) with `tokentype: API_KEY`. They skip OAuth refresh and do not use `profileArn`.

## Operations

See [docs/operator-runbook.md](docs/operator-runbook.md) for production health checks, config backup/restore, credential recovery, account/model diagnostics, request replay dry-runs, logs, metrics, and deployment verification steps. For external IdP import preview, conflict decisions, diagnostics, and audit behavior, see [docs/external-idp-import.md](docs/external-idp-import.md). For upstream Kiro-issued API keys, automatic/manual profile and region selection, hosted-SSO profile choice, model detection/routing, admin endpoints, migrations, and secret-bearing exports, see [docs/kiro-api-key-and-profiles.md](docs/kiro-api-key-and-profiles.md). For per-request tracing — failover attempt detail, capture modes, what is scrubbed, retention, the query API, and a 60-second debugging walkthrough — see [docs/request-tracing.md](docs/request-tracing.md).

## Thinking Mode
When any API key is enabled, requests must carry a valid key (`Authorization: Bearer sk-...`).

### Endpoints

| Type | Paths |
|------|-------|
| API (key-gated) | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`, `/v1/responses`, `/v1/models`, `/v1/stats` |
| Self-service (caller's own key) | `/v1/key/info`, `/v1/key/logs`, `/usage` |
| Admin | `/admin` (page), `/admin/api/*` (password-gated) |
| Public | `/health`, `/check` |

### Model overrides & identity

- **Force Model** (Settings → Force Model): overrides the model of *every* request. Use it when a client asks for a model name that doesn't exist upstream (e.g. remap `claude-sonnet-4.8` → a real model) to avoid 503s.
- **Per-key Model** (API-key modal): same idea, scoped to one key. Force Model takes precedence.
- **Identity Model**: only changes how the assistant answers "what model are you?" — it does not change the upstream model.

## Importing Microsoft 365 / Entra ID (Azure AD) credentials

Enterprise SSO (Microsoft 365 / Entra ID) accounts are neither AWS Builder ID nor
IAM Identity Center accounts, so they are minted through the interactive browser
sign-in helper `kiro-login-helper.py`, which writes a `CLIProxyAPI_<user>.json`
credential file (`auth_method: external_idp`). There are three ways to load that file:

1. **Paste / upload in the admin panel.** Add Account → Credentials JSON (or the
   Enterprise SSO card's file picker) accepts the helper's native `CLIProxyAPI_*.json`
   verbatim — snake_case keys (`token_endpoint`, `issuer_url`, `scopes`, `profile_arn`)
   are understood.

2. **API.** `POST /admin/api/auth/import-cli-json` accepts a single helper object, a
   JSON array, a `{ "files": ["<json>", ...] }` / `{ "accounts": [...] }` wrapper, or
   raw text with several objects. It returns per-item results.

   ```bash
   curl -X POST http://localhost:8080/admin/api/auth/import-cli-json \
     -H "X-Admin-Password: $ADMIN_PASSWORD" \
     --data-binary @CLIProxyAPI_user.json
   ```

3. **Zero-touch drop folder (Docker).** With `KIRO_IMPORT_WATCH=1` (set by default in
   `docker-compose.yml`), any `CLIProxyAPI_*.json` placed in `data/imports/` is imported
   within ~15s, then moved to `data/imports/processed/` (or `failed/` with a `.error.txt`
   sidecar). Imports go through the same persisted path the running server owns, so they
   never race the in-memory config.

4. **Import from the Kiro IDE cache (no browser, no helper).** If the Kiro IDE is already
   signed in on the same host as the proxy, it keeps a live credential at
   `~/.aws/sso/cache/kiro-auth-token.json`. The admin panel's Enterprise SSO card has an
   **Import from Kiro IDE (this host)** button, or call the API directly:

   ```bash
   curl -X POST http://localhost:8080/admin/api/auth/import-ide-cache \
     -H "X-Admin-Password: $ADMIN_PASSWORD"
   # custom location: -d '{"path":"/path/to/kiro-auth-token.json"}'
   ```

   The proxy reads the file **server-side**, so this works only when the IDE and the proxy
   share a host — or, in Docker, when the host AWS SSO cache directory is mounted into the
   container. The Compose file does this portably for Linux/macOS with
   `${HOME}/.aws/sso/cache:/host-aws-sso-cache:ro`; override `KIRO_AWS_SSO_CACHE_DIR` if
   your Kiro IDE uses a different location. The cache's stale `expiresAt` is ignored: the
   import performs a mandatory refresh, so the persisted expiry always comes from a fresh
   upstream response.

> The account email is stored as a label only. The password is **never** persisted or
> sent upstream — Microsoft 365 tenants enforce MFA / Conditional Access, so a headless
> password (ROPC) grant is not a reliable auth path. Use the interactive helper to mint
> the credential, then import the JSON.

## Environment Variables
### Thinking mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude requests with a top-level `thinking` config (`{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}`) also enable it. Output format is configurable in Settings → Thinking Mode.

### Outbound proxy

Configure in Settings → Outbound Proxy. Supports SOCKS5 and HTTP. This fork adds a **shared proxy pool** with health tracking and failover, and a **Require-proxy** toggle that blocks outbound Kiro requests if no proxy is available (prevents leaking the server's real IP). Changes take effect immediately without a restart.

## Environment variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config) | - |
| `PORT` | HTTP listen port (overrides config; `-port` flag wins over this) | `8080` |
| `HOST` | HTTP bind host (overrides config; `-host` flag wins over this) | `127.0.0.1` |
| `KIRO_IMPORT_WATCH` | Enable the `data/imports/` auto-ingest watcher (`1`/`true`) | off (on in Docker) |
| `KIRO_IMPORT_DIR` | Directory the watcher scans for `CLIProxyAPI_*.json` | `data/imports` |
| `KIRO_IDE_CACHE` | Path to the Kiro IDE credential cache for `import-ide-cache` | `~/.aws/sso/cache/kiro-auth-token.json` (Docker: `/host-aws-sso-cache/kiro-auth-token.json`) |
| `KIRO_AWS_SSO_CACHE_DIR` | Host AWS SSO cache directory mounted by Docker Compose for IDE-cache import | `$HOME/.aws/sso/cache` |
| `KIRO_PROFILE_REGIONS` | Comma-separated fallback regions for profile discovery and Kiro API-key probing | `us-east-1,eu-central-1` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config at startup) | - |
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `LOOPBACK_HOST` | Host to bind the SSO loopback server. **Set to `0.0.0.0` in Docker.** | `127.0.0.1` |
| `KIRO_MAX_BODY_BYTES` | Max request body size (`0` = disable) | `10485760` (10 MiB) |
| `KIRO_MAX_CONCURRENT` | Global concurrent-request cap | `256` |
| `KIRO_IP_RPM` | Requests/minute/IP before reject | `120` |
| `KIRO_PER_KEY_INFLIGHT` | Concurrent RPM-delayed requests per key before 429 | `8` |
| `KIRO_TRUST_PROXY` | Read the real client IP from `X-Forwarded-For` / `X-Real-IP`. Only enable behind a trusted reverse proxy. | `false` |

## Troubleshooting

**`Refusing to start: admin password is still the default on a non-loopback host`.** This is a deliberate safety guard: the app won't boot when the admin password is still `changeme` **and** the host is not loopback (e.g. `0.0.0.0`). Two fixes: for local use, set `"host": "127.0.0.1"` in `data/config.json` (you can keep `changeme`); for a public/Docker deploy, set a strong `ADMIN_PASSWORD` env var instead. Never expose `0.0.0.0` with the default password.

**"All loopback ports busy" / Start Login returns HTTP 500.** Usually a bad `LOOPBACK_HOST` (e.g. `0.0.0` missing an octet). In Docker, it must be exactly `0.0.0.0`. Check with `docker compose exec kiro-go printenv LOOPBACK_HOST`, fix the compose file, then `docker compose up -d --force-recreate` (env is baked at container creation). Starting a new login also cancels pending sessions to free leaked loopback ports.

**App runs an old version after a code change.** `docker compose up` won't rebuild a cached image. Run `docker compose up -d --build`.

**Compose fails on port `49153`/`5015x` ("address already in use") on macOS.** Those ports are in the OS ephemeral range. They're unnecessary — only the low SSO ports (3128–9091) are needed. Remove the `49153`–`53153` mappings.

**Getting 503 on a specific model.** The client is likely requesting a model that doesn't exist upstream. Use Force Model or per-key Model to remap it to a real one.

**Per-IP rate limits look wrong behind a proxy.** If you run Nginx/Cloudflare in front, set `KIRO_TRUST_PROXY=true` — otherwise every request looks like it comes from `127.0.0.1`. If you're exposing Go directly, keep it `false`, or attackers can spoof `X-Forwarded-For`. See [deploy/HARDENING.md](deploy/HARDENING.md).

**Lost config / accounts after a crash.** Config lives in `data/config.json`; mount `/app/data` as a volume so it survives container recreation. Always run behind the `./data` volume in `docker-compose.yml`.

For production DoS/DDoS hardening (Cloudflare + Nginx + fail2ban), see [deploy/HARDENING.md](deploy/HARDENING.md).

## Development

```bash
go build -o kiro-go .          # build
go test ./...                  # run all tests
go test ./proxy/               # test one package
go vet ./...                   # vet
```

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
