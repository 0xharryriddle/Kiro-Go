# Changelog

All notable changes to this project are documented here. The format loosely
follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Fixed

- Made credential replacement a single durable config transaction, rolled back ordinary account updates on save failure, rejected cross-region profile discovery mismatches, and prevented stale admin profile/API-key responses from rendering after modal changes.

### Added
- **Upstream Kiro-issued API-key authentication.** Accounts can now authenticate to
  Kiro with a recoverable `ksk_...` credential stored only in `kiroApiKey`, using the
  upstream `Authorization: Bearer` / `TokenType: API_KEY` contract without entering
  OAuth refresh. A five-minute probe/commit flow validates candidate regions before
  persistence, returns sanitized metadata, commits one selected region, and atomically
  deduplicates the same Kiro identity plus region.
- **Kiro API-key admin UI and import/export round trip.** Add Account now includes a
  Kiro API Key method with region probe results and masked account presentation.
  Credential import live-validates `authMethod: api_key` records; explicit credential
  export includes the key and effective region while ordinary account lists expose only
  credential type, presence, and a mask.
- **Existing-account multi-profile discovery and switching.** New authenticated
  `GET/POST /admin/api/accounts/{id}/kiro-profiles` and
  `POST /admin/api/accounts/{id}/kiro-profiles/auto` endpoints discover profiles across
  deterministic candidate regions, report partial-region warnings, validate selections
  against a fresh result, and atomically align a manual profile with its data-plane region.
- **Hosted-SSO multi-profile choice.** Zero/one/multiple profile outcomes now preserve
  lazy fallback, auto-cache one profile, or park the exchanged credential for explicit
  selection. Pending credentials are TTL-bound, retry-safe after persistence failures,
  consume-once after success, and cleared by cancellation. Overlapping polls and cancel
  are serialized across the auth-session-to-choice handoff.
- **Profile-aware model detection and routing.** Profile/region changes invalidate stale
  per-account and aggregate model caches, refresh the selected profile's model list, and
  route requested models only to compatible accounts. Non-pinned profiles can self-heal
  once across regions after profile/plan authorization failures.
- Added [docs/kiro-api-key-and-profiles.md](docs/kiro-api-key-and-profiles.md) covering
  credentials, admin APIs, automatic region/profile selection, model routing, security
  boundaries, migration, rollback, and the deferred multi-region-slot design.
- **Import Microsoft 365 / Entra ID (Azure AD) credentials from the login helper.**
  Three converging paths now load a `CLIProxyAPI_*.json` file produced by
  `kiro-login-helper.py`, all funnelling through one `importOne` core so the
  persisted account is identical to an interactive Enterprise SSO login:
  - `apiImportCredentials` (`POST /admin/api/auth/credentials`) now understands the
    `external_idp` auth method and accepts the helper's native snake_case keys
    (`token_endpoint`, `issuer_url`, `scopes`, `profile_arn`) in addition to the
    existing camelCase payload.
  - New `POST /admin/api/auth/import-cli-json` endpoint ingests a single helper
    object, a JSON array, a `{ "files": [...] }` / `{ "accounts": [...] }` wrapper,
    or raw text with several objects, returning per-item results.
- `KIRO_IMPORT_WATCH` / `KIRO_IMPORT_DIR` zero-touch drop-folder watcher
  (`data/imports/`): valid files are imported within ~15s through `config.AddAccount`
  then moved to `processed/`, invalid ones to `failed/` with a `.error.txt` sidecar.
  Enabled by default in `docker-compose.yml`.
- Admin panel file picker for uploading helper JSON; `app.js` credential parsing now
  maps both snake_case and camelCase.
- `testdata/CLIProxyAPI_sample_external_idp.json` sanitized fixture.
- **Import directly from the Kiro IDE cache (no browser, no helper script).** New
  `POST /admin/api/auth/import-ide-cache` endpoint and an admin-panel button read the
  credential the Kiro IDE already cached on the host
  (`~/.aws/sso/cache/kiro-auth-token.json`, overridable via `KIRO_IDE_CACHE` or a `path`
  body field) and import it through the same `importOne` core. The cache's camelCase keys
  map straight onto the existing decoder; its stale `expiresAt` is ignored in favor of the
  mandatory refresh.
- **Configurable listen port/host.** New `-port` / `-host` CLI flags and `PORT` /
  `HOST` env overrides (precedence: flag > env > `config.json`), so the proxy can run
  on a port other than `8080` without editing the config. `docker-compose.yml` honors
  `KIRO_PORT` for the published host port (`KIRO_PORT=9090 docker compose up -d`).
- **Operator health and recovery tooling.** Added `/healthz`, `/readyz`, config status /
  backup / restore / export APIs, rolling config backups, and atomic config writes with
  startup recovery from valid backups.
- **Admin diagnostics and observability.** Added persistent request logs, metrics summary,
  server-side log filtering/export, account diagnostics, model routing diagnostics, and a
  safe request replay dry-run endpoint for validating payloads without upstream calls.
- **Operator runbook.** Added `docs/operator-runbook.md` with deployment verification,
  troubleshooting, credential recovery, diagnostics, logs, metrics, and security checks.

### Fixed
- Profile ARN, manual-pin state, and data-plane region now form an atomic routing tuple.
  Stale whole-account writers cannot overwrite a concurrent manual selection, and failed
  durable writes roll back in-memory add, replacement, region, and profile mutations.
- Kiro API-key and hosted-SSO commits retain the original pending credential and deadline
  after a durable-write failure, allowing safe retry without re-entering secrets or login.
- `external_idp` imports previously returned `400 "external IdP refresh requires
  clientId and tokenEndpoint"` because the import endpoint dropped `tokenEndpoint`,
  `issuerUrl`, `scopes`, and `profileArn`. The refresh-before-import step now carries
  the external IdP material so the refresh against the IdP token endpoint succeeds,
  and an `external_idp` import that omits `token_endpoint`/`client_id` is rejected
  up front with an actionable message instead of an opaque refresh failure.

### Security
- Kiro API keys are never mirrored into OAuth `accessToken`, returned by probe/commit or
  ordinary account-list responses, or included in sanitized errors. Probe, commit, full
  account, credential export, and config export responses disable caching. Full-account,
  credential-export, config-export, and backup surfaces remain intentionally secret-bearing
  operator channels and must be protected accordingly.
- Legacy Kiro-key records normalize on load: `apikey` becomes `api_key`, duplicate
  `accessToken == kiroApiKey` material is removed, and keyless API-key rows are disabled.
- The account email is stored as a label only; the password is never persisted or
  sent upstream. Microsoft 365 tenants enforce MFA / Conditional Access, so a headless
  ROPC password grant is not a supported auth path — the interactive helper (browser
  PKCE) remains the canonical credential-mint path.
