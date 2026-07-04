# Operator Runbook

This runbook covers production checks and recovery workflows for Kiro-Go operators. All admin API examples require the admin password through `X-Admin-Password`.

```bash
export BASE_URL=http://127.0.0.1:8080
export ADMIN_PASSWORD=changeme
```

## Startup and Health Checks

Use `/healthz` for liveness and `/readyz` for readiness.

```bash
curl -fsS "$BASE_URL/healthz"
curl -fsS "$BASE_URL/readyz"
```

Expected readiness checks include config readability, config writability, imports directory writability, account availability, and web asset presence. If `/healthz` works but `/readyz` fails, inspect the JSON body first; it points to the failing readiness component.

## Config Safety

Kiro-Go writes `data/config.json` atomically and keeps rolling backups at `data/config.json.bak.1` through `.bak.5`.

Useful admin endpoints:

```bash
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/config/status"
curl -fsS -X POST -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/config/backup"
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/config/export" -o config-export.json
```

Restore the latest backup from the admin panel when possible. Prefer the API/UI over editing `config.json` while the service is running; runtime config writes can overwrite manual edits.

## Credential Recovery

The Credential Recovery Wizard supports preview-first recovery so operators can confirm metadata before importing secrets.

Available flows:

- Kiro IDE cache preview/import from the host cache mount.
- CLI helper JSON preview/import from `CLIProxyAPI_*.json`.
- Docker auto-ingest from `data/imports/` when `KIRO_IMPORT_WATCH=1`.

Preview endpoints expose safe metadata only and do not persist accounts or return tokens.

```bash
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" \
  "$BASE_URL/admin/api/auth/import-ide-cache/preview"
```

For Docker auto-ingest, drop helper JSON into `data/imports/`. Processed files move to `data/imports/processed/`; failed files move to `data/imports/failed/` with a `.error.txt` reason.

## Account and Model Routing Diagnostics

Account diagnostics explain why accounts are or are not routable without calling upstream APIs.

```bash
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" \
  "$BASE_URL/admin/api/accounts/diagnostics"
```

Model routing diagnostics answer which accounts can route a requested model using cached model lists.

```bash
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" \
  "$BASE_URL/admin/api/models/routing?model=claude-sonnet-4.5"
```

Important reason codes:

- `available`: account is currently routable.
- `disabled`: account is disabled.
- `cooldown`: account is locally cooling down.
- `token_expiring`: token is too close to expiry.
- `quota_exhausted`: usage limit is reached and overage is not enabled.
- `unsupported_model`: account has a cached model list that excludes the requested model.
- `not_in_pool`: account exists in config but is not in the active routing pool.

Refresh model caches from the admin panel after adding accounts or when upstream model access changes.

## Request Replay Diagnostics

Use replay diagnostics to validate request shape, token estimate, and routing without sending an upstream request.

```bash
curl -fsS -X POST \
  -H "X-Admin-Password: $ADMIN_PASSWORD" \
  -H "Content-Type: application/json" \
  "$BASE_URL/admin/api/replay/diagnose" \
  -d '{
    "endpoint": "openai",
    "payload": {
      "model": "claude-sonnet-4.5",
      "messages": [{"role":"user","content":"hello"}],
      "max_tokens": 64
    }
  }'
```

A valid response includes `dryRun: true`. This endpoint is safe for production troubleshooting because it does not call Kiro upstream and does not consume quota.

## Logs and Metrics

Request logs are kept in memory and persisted to `data/request_logs.json`. The cache is capped to the latest 500 entries.

```bash
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/logs"
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/logs?status=error&q=quota"
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/logs?format=csv" -o request-logs.csv
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/metrics/summary"
```

Clear logs from the admin panel or with:

```bash
curl -fsS -X DELETE -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/logs"
```

## Security Checks

The admin panel displays an operator warning when the admin password is missing, weak, or still set to `changeme`.

```bash
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" \
  "$BASE_URL/admin/api/security/status"
```

Before exposing the service:

1. Change the admin password to at least 12 characters.
2. Enable API key enforcement for client API traffic.
3. Mount `/app/data` persistently.
4. Keep the admin panel behind a trusted network, VPN, reverse proxy auth layer, or firewall.

## Deployment Verification Checklist

After each deploy or container rebuild:

```bash
docker compose ps
curl -fsS "$BASE_URL/healthz"
curl -fsS "$BASE_URL/readyz"
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/config/status"
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/accounts/diagnostics"
curl -fsS -H "X-Admin-Password: $ADMIN_PASSWORD" "$BASE_URL/admin/api/metrics/summary"
```

Then run one replay dry-run and, only if needed, one real low-token client request.

## Troubleshooting

- Config status fails: inspect `data/config.json` and backup files, then restore from the admin panel.
- Readyz reports `accounts: 0`: import or enable at least one account, then refresh the account pool by saving settings or restarting.
- Model routing returns `unsupported_model`: refresh that account's model cache or choose a model supported by the account.
- Logs show repeated quota errors: verify upstream quota/overage status; local retries cannot recover upstream throttling.
- Docker starts but host API is unreachable: confirm the host publishes port `8080` and the service inside the container listens on the expected port.
