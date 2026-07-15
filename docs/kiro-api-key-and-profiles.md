# Kiro API Keys, Profiles, Regions, and Models

This guide documents upstream Kiro-issued API-key authentication, automatic and manual profile selection, regional routing, model discovery, hosted-SSO profile choice, migration behavior, and the associated admin APIs.

Do not confuse an upstream Kiro API key (`ksk_...`) with a downstream proxy API key (`sk-...`). A downstream key authenticates clients calling Kiro-Go. An upstream Kiro key authenticates Kiro-Go to Kiro and must remain recoverable at rest.

## Credential Types

Kiro-Go supports these upstream account credentials:

- OAuth/social and AWS IAM Identity Center accounts use `accessToken` and may refresh with `refreshToken`.
- External IdP accounts use OAuth tokens plus their external token endpoint and client material.
- Kiro API-key accounts use `authMethod: "api_key"` and store the key only in `kiroApiKey`. The key is never copied into `accessToken` and never enters OAuth refresh.

Upstream requests made with a Kiro key use `Authorization: Bearer <ksk_...>` and `TokenType: API_KEY`. External IdP behavior remains `TokenType: EXTERNAL_IDP`.

## Add a Kiro API-Key Account

The admin panel provides **Add Account -> Kiro API Key**. Onboarding has two stages:

1. Probe the requested regions. Kiro-Go validates the key, calls Kiro independently in each region, and returns only sanitized availability, identity, subscription, and usage metadata.
2. Commit one successful region. The key is held only in a five-minute, consume-once in-memory probe until the selected account is durably persisted.

The key input is cleared after submission. Failed regions return categorized codes such as `unauthorized`, `rate_limited`, `region_unavailable`, or `upstream_error`; raw upstream bodies and the key are not returned.

API equivalents (all require normal admin authentication):

```http
POST /admin/api/auth/kiro-api-key/probe
Content-Type: application/json

{
  "kiroApiKey": "ksk_...",
  "regions": ["us-east-1", "eu-central-1"]
}
```

A successful probe returns `probeId`, `expiresAt`, and per-region results. Commit one result:

```http
POST /admin/api/auth/kiro-api-key/commit
Content-Type: application/json

{
  "probeId": "...",
  "selectedRegion": "eu-central-1",
  "nickname": "Primary Kiro key",
  "enabled": true
}
```

A commit is consume-once after success. A durable-write failure retains the same probe and original deadline so the request can be retried. Duplicate underlying identity plus selected region returns HTTP 409 and does not create duplicate routing capacity.

When `regions` is omitted, probing uses `KIRO_PROFILE_REGIONS`, or the built-in fallback list. At most ten regions may be requested.

## Import and Export

Credential import accepts a Kiro key only when:

- `authMethod` is `api_key`;
- `kiroApiKey` starts with `ksk_` and passes validation;
- `region` is a valid selected data-plane region; and
- no OAuth token, refresh, client, or token-endpoint material is mixed into the record.

Imports perform a live Kiro probe before persistence and apply the same identity-plus-region deduplication rule as interactive onboarding.

Explicit credential export includes `kiroApiKey` and the selected effective region so the account can round-trip. Ordinary account listings expose only `credentialKind`, `hasToken`, and a masked key suffix.

## Secret-Bearing Admin Surfaces

The following authenticated operator surfaces intentionally contain recoverable credentials and must be protected like `data/config.json`:

- full-account detail responses;
- credential export;
- config export and rotating config backups.

These responses use `Cache-Control: no-store`. Do not expose the admin endpoint publicly, log response bodies, or distribute exports without encryption. Ordinary account-list, probe, commit, profile-discovery, and error responses do not reveal the full Kiro key.

## Automatic Profile and Region Selection

For OAuth and external-IdP accounts, Kiro-Go resolves profiles across candidate regions in deterministic order:

1. region embedded in the current profile ARN;
2. current data-plane `regionOverride`;
3. the account authentication `region`;
4. `KIRO_PROFILE_REGIONS`, or built-in fallback regions.

If one profile exists, it is cached automatically. If multiple profiles exist, Kiro-Go probes usage and prefers a profile attached to a usable subscription plan. The selected profile ARN determines the regional data-plane host. Authentication region and data-plane region remain separate.

Automatic resolution is lazy when needed by usage, generation, or model discovery. If a cached automatic profile produces a profile/plan authorization failure, Kiro-Go re-runs plan-aware cross-region discovery and retries once. Manually pinned profiles are never silently replaced.

`KIRO_PROFILE_REGIONS` is a comma-separated discovery/probe list, for example:

```bash
KIRO_PROFILE_REGIONS=us-east-1,eu-central-1,ap-southeast-1
```

## Discover and Switch Profiles

The account detail panel provides **Discover profiles**, **Select**, and **Automatic selection** controls.

- Discovery scans all candidate regions without treating the current region pin as a search restriction.
- Profiles are deduplicated by ARN and marked current, pinned, and usable.
- Partial region failures are returned as warnings rather than hiding successful profiles.
- Selecting a profile performs fresh discovery immediately before mutation. Caller-supplied or stale ARNs that are not in that result are rejected.
- Manual selection atomically stores `profileArn`, `profilePinned: true`, and an aligned `regionOverride` derived from the ARN.
- Automatic selection atomically clears all three fields, resolves a fresh automatic profile, reloads routing, and refreshes models.
- Editing the standalone region override exits manual-profile mode and forces automatic profile resolution inside the selected region.

Admin endpoints:

```text
GET  /admin/api/accounts/{id}/kiro-profiles
POST /admin/api/accounts/{id}/kiro-profiles       {"profileArn":"arn:..."}
POST /admin/api/accounts/{id}/kiro-profiles/auto
```

Kiro API-key accounts return profile mode `key_bound`; their selected region controls the host and no listable profile ARN is required.

## Hosted-SSO Profile Choice

After hosted SSO token exchange, Kiro-Go discovers usable profiles before creating the account:

- zero profiles: persist the credential and preserve lazy profile resolution;
- one profile: cache it in automatic mode;
- multiple profiles: hold the exchanged credential in memory for five minutes and require an explicit choice.

Repeated or overlapping poll requests return the same pending choice without re-exchanging credentials. Cancellation is serialized with poll completion and removes both the auth session and pending choice. Finalization accepts only an offered ARN, stores the original absolute token expiry, and consumes the credential only after durable persistence. A persistence failure retains the original credential and deadline for retry.

Finalize endpoint:

```http
POST /admin/api/auth/kiro-sso/profile
Content-Type: application/json

{"sessionId":"...","profileArn":"arn:..."}
```

## Model Detection and Routing

Model discovery always uses the account's resolved profile and regional data-plane host. Kiro-Go caches the upstream model IDs per account and builds the aggregate `/v1/models` response from those caches.

After a profile or region switch, the old per-account model cache and aggregate cache are invalidated, the pool is reloaded, and models are refreshed. A profile/plan authorization failure during `ListAvailableModels` triggers one automatic profile self-heal and retry for non-pinned accounts.

Incoming requests are routed by their requested model. An account is eligible only when its discovered model cache and optional operator `modelAllowList` permit that model. Before the first model refresh, routing is intentionally optimistic except for explicit allow-list exclusions.

This is capability detection, not prompt-content inference: Kiro-Go does not inspect conversation meaning, programming language, or desired context window to choose a profile or model automatically.

## Configuration Migration and Rollback

On load, Kiro-Go normalizes legacy Kiro-key records:

- `authMethod: "apikey"`, or an empty method with `kiroApiKey`, becomes `api_key`;
- a duplicated `accessToken` equal to `kiroApiKey` is cleared;
- an `api_key` record without `kiroApiKey` is disabled instead of falling back to OAuth behavior.

Profile routing uses an atomic tuple: `profileArn`, `profilePinned`, and `regionOverride`. Ordinary whole-account updates preserve this tuple so stale usage or refresh snapshots cannot overwrite a concurrent operator selection. Durable-write failures roll back in-memory account/profile mutations.

Older binaries ignore the new JSON fields but cannot serve Kiro API-key accounts. Disable those accounts or retain a compatible binary before rollback. Keep protected config backups during upgrades.

## Deferred Multi-Region Slots

One Kiro API-key account intentionally represents one selected region. Creating simultaneous route slots in several regions would duplicate one long-lived secret and one quota identity across account rows. That feature is deferred until credentials are normalized into a separate entity referenced by route/profile slots.
