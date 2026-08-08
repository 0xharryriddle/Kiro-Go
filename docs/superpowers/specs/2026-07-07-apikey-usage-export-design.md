# API Key Usage Export — Design

Date: 2026-07-07

## Goal

Add an export feature for proxy API keys that produces a **usage report** (not a
backup). Keys are always masked; the export cannot be re-imported. It surfaces per-key
state and cumulative usage (requests / tokens / credits) plus derived columns, in both
JSON and CSV.

This is the proxy API keys managed in `config/apikeys.go` (`ApiKeyEntry`), NOT the Kiro
upstream account credentials (which already have their own `/export`).

## Scope

- New admin endpoint that returns a masked usage report as JSON.
- New "Export" button + modal in the API Keys tab, mirroring the existing account
  export modal (select individual keys, toggle all, show/copy JSON, download JSON,
  download CSV).
- CSV generated frontend-side from the same JSON payload (single data source).

Out of scope: re-import, unmasked key values, backup/round-trip.

## Backend

New handler in `proxy/admin_apikeys.go`, wired in `handler.go` routing next to the other
`/api-keys` cases.

Route: `POST /admin/api/api-keys/export` (password-gated like all admin API).

Request body:
```json
{ "ids": ["id1", "id2"] }
```
Empty or missing `ids` = export all (matches account `/export` behavior).

Response:
```json
{
  "version": "<config version string>",
  "exportedAt": 1751000000,
  "apiKeys": [
    {
      "id": "…",
      "name": "…",
      "keyMasked": "sk-***abc",
      "enabled": true,
      "requestsCount": 123,
      "tokensUsed": 45678,
      "creditsUsed": 12.5,
      "tokenLimit": 100000,
      "creditLimit": 50,
      "expiresAt": 0,
      "createdAt": 1750000000,
      "lastUsedAt": 1750500000,
      "tokenPercentUsed": 45.68,
      "creditPercentUsed": 25.0,
      "overToken": false,
      "overCredit": false,
      "expired": false
    }
  ]
}
```

Rules:
- Key value always masked via `config.MaskApiKey`. Never emit the raw key.
- `id` is included.
- Filter by `ids` when provided; otherwise all entries from `config.ListApiKeys()`.
- Derived fields computed server-side:
  - `tokenPercentUsed` = `tokensUsed / tokenLimit * 100`, `0` when `tokenLimit == 0`.
  - `creditPercentUsed` = `creditsUsed / creditLimit * 100`, `0` when `creditLimit == 0`.
  - `overToken`, `overCredit` from `config.ApiKeyOverLimit`.
  - `expired` from `config.ApiKeyExpired`.
- `exportedAt` = `time.Now().Unix()`.
- Read-only: no config writes.

A dedicated struct (e.g. `apiKeyExportView`) extends the existing masked fields with the
derived columns. Reuse `config.ApiKeyOverLimit` / `config.ApiKeyExpired` so limit/expiry
logic stays in one place.

## Frontend

`web/index.html`: add an "Export" button in the API Keys list toolbar, next to
`bulkAddApiKeyBtn` / `addApiKeyBtn`.

`web/app.js`: an export modal mirroring the account export modal
(`showExportModal`/`renderExportModal`/`getExportData`/`exportShowJson`/`exportCopyJson`/
`exportDownloadJson`), reusing the existing `export-*` CSS classes. Differences:
- Rows list API keys (name + masked key + enabled/usage summary) instead of accounts.
- Data source: `POST /api-keys/export` with selected ids.
- Buttons: Cancel, Show JSON, Copy JSON, Download JSON, **Download CSV**.

CSV built client-side from the JSON payload:
- Header row, one key per data row.
- Columns, in order: `id, name, keyMasked, enabled, requestsCount, tokensUsed,
  creditsUsed, tokenLimit, creditLimit, tokenPercentUsed, creditPercentUsed, overToken,
  overCredit, expired, expiresAt, createdAt, lastUsedAt`.
- Escape fields containing comma / quote / newline by wrapping in `"` and doubling inner
  `"`.
- Numbers written raw (no thousands separators) so spreadsheets parse them; the on-screen
  JSON preview may format large numbers for readability, but CSV/JSON downloads stay raw.
- Timestamps left as raw Unix seconds (consistent with account export, which emits epochs).

## i18n

Add `apiKeys.export.*` keys to each locale file under `web/locales`, mirroring the
existing `export.*` keys used by the account export modal (title, selected count,
select/deselect all, show/copy/download JSON, no-selection warning, copied toast) plus a
new `downloadCsv` label.

## Testing

One Go test in `proxy/` for the new endpoint:
- Seed a few API key entries via config (with/without limits, one over-limit, one
  expired).
- Call the handler; assert:
  - key value is masked (raw `sk-` value not present in output),
  - `ids` filter returns only requested entries,
  - `tokenPercentUsed` / `creditPercentUsed` computed correctly, `0` when limit is 0,
  - `overToken` / `overCredit` / `expired` flags match expectations.

Use existing config test setup patterns (see `config/config_test.go` / existing proxy
tests) for initializing config in-memory.

## Files touched

- `proxy/admin_apikeys.go` — new export handler + view struct.
- `proxy/handler.go` — route registration.
- `proxy/admin_apikeys_export_test.go` (new) — endpoint test.
- `web/index.html` — export button.
- `web/app.js` — export modal + CSV builder.
- `web/locales/*` — `apiKeys.export.*` strings.
