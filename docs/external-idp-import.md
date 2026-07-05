# External IdP Import and Recovery

Kiro-Go accepts external IdP credentials from the admin Tools tab and from the credential import APIs. The import pipeline is shared by normal imports, helper JSON imports, IDE cache previews, and watcher-driven imports so preview and apply behavior stays aligned.

## Supported Input Shapes

- Raw helper JSON using snake_case fields such as `auth_method`, `refresh_token`, `token_endpoint`, and `issuer_url`.
- Admin/API JSON using camelCase fields such as `authMethod`, `refreshToken`, `tokenEndpoint`, and `issuerUrl`.
- Arrays of helper JSON documents.
- Wrapped credential objects normalized by the existing CLI JSON import helpers.

## Preview Before Import

Use the Tools tab Credential Recovery Wizard to paste JSON and preview it before import. Preview never persists accounts and never displays secret values. It only exposes safe metadata:

- normalized auth method and provider
- detected email label
- token endpoint and issuer URL
- whether refresh/access/client material is present
- endpoint validation result
- derived field source, when applicable
- duplicate ID or same-email conflicts
- trust-on-import warnings for JWT access tokens with `exp`

## Endpoint Validation

External IdP endpoints are validated before import:

- HTTPS is required.
- IP literal endpoints are rejected.
- unsupported hosts are rejected.
- known Microsoft login hosts are accepted by the auth validator.

If `tokenEndpoint`, `issuerUrl`, or scopes are missing, Kiro-Go attempts derivation from `userId` or the access token issuer before validating the result.

## Import Decisions

The apply endpoint supports explicit decisions per item:

- `create_new` imports as a new local account.
- `skip` ignores the item.
- `replace_existing` validates the new credential, then replaces the selected account ID with the validated material.

The UI currently presents conflicts in preview and continues to use the established import path for normal pasted helper JSON. The apply API is available for safer resolver workflows.

## Diagnostics

The Tools tab includes External IdP Diagnostics:

- Static checks do not perform network refresh.
- Static checks report missing refresh material, rejected endpoints, refresh windows, missing profile ARN, and local routing state.
- Live refresh check is explicit and refreshes one selected external IdP account.

## Account Usage Audit (External-Usage Detection)

The Tools tab includes an Account Usage Audit panel that estimates whether each
credential is being used **outside this proxy** — for example by the real Kiro
IDE, a second proxy, or another person the credential was shared with.

### How it works

Two independent usage numbers exist per account:

- **Upstream authoritative usage** — `getUsageLimits` (AWS Q) reports the credits
  the credential burned this billing period from *any* client. Refreshed every 30
  minutes and on an explicit recheck.
- **Our usage** — the credits Kiro-Go itself metered (from the `meteringEvent`
  stream) for requests it proxied, accumulated per billing period.

Both use the **same AWS metering unit (agentic-request credits)**, so:

```
externalCredits = (upstreamCurrent - periodStart) - ourPeriodCredits   (clamped >= 0)
```

The signed delta is accumulated across the period and clamped to zero only at
display, so metering lag (a credit we posted before upstream accounted for it)
averages out instead of biasing the estimate upward.

### Units — important

This audit is expressed in **credits only**. Upstream exposes **no token count**
for traffic Kiro-Go did not originate, so there is deliberately no "external
tokens" figure — it would be fabricated. The only honest token number is
*our tokens*, which covers only requests this proxy served.

### Confidence tiers

| Tier | Meaning |
|---|---|
| `clean` | External usage within tolerance — only this proxy uses the credential |
| `external` | External usage materially positive — credential used elsewhere too |
| `strong_external` | Account disabled for local routing yet upstream grew — unambiguous third-party use |
| `unknown` | No upstream data yet, a mid-period import with no baseline, or a just-reset period |

Tolerance is the greater of a small absolute floor (1 credit) or 5% of in-period
upstream growth, to avoid flapping on metering noise.

### Period rollover

When the billing period changes (`nextResetDate` differs) or upstream usage drops
below the recorded baseline (a reset we missed), the baseline is re-captured, the
in-period accumulator is zeroed, and the verdict is `unknown` for that one cycle.
Usage from a prior period is never miscounted as external, and a verdict is never
emitted from an incomplete baseline. A freshly imported account therefore shows
`unknown` until the next reset — honest by design.

### Recheck

`GET /admin/api/accounts/usage-audit` returns the cached fleet verdict and makes
no upstream calls. `POST /admin/api/accounts/usage-audit/recheck` (optionally with
`{"accountId":"..."}`) forces a live upstream fetch and recompute. A first
transition into `external`/`strong_external` emits a `security` /
`external_usage_detected` audit event.

## Audit Logs

Audit logs record safe operational events for previews, imports, replacements,
live diagnostics, and external-usage detection. They do not store refresh tokens,
access tokens, client secrets, or raw pasted JSON.

## Troubleshooting

| Symptom | Meaning | Fix |
|---|---|---|
| endpoint rejected | Token or issuer endpoint failed validator | Verify tenant URL and host |
| missing refresh material | Refresh token, client ID, or token endpoint is absent | Export a complete Kiro credential or derive from `userId` |
| trust-on-import warning | Access token JWT `exp` was used without live refresh | Run External IdP Diagnostics after import |
| profile ARN missing | Account can still import but profile will be resolved lazily | Run a live check or first request |
| disabled locally | Kiro-Go will not route requests through that account | Enable local routing in Accounts |
