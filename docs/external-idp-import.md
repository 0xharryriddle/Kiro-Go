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

## Audit Logs

Audit logs record safe operational events for previews, imports, replacements, and live diagnostics. They do not store refresh tokens, access tokens, client secrets, or raw pasted JSON.

## Troubleshooting

| Symptom | Meaning | Fix |
|---|---|---|
| endpoint rejected | Token or issuer endpoint failed validator | Verify tenant URL and host |
| missing refresh material | Refresh token, client ID, or token endpoint is absent | Export a complete Kiro credential or derive from `userId` |
| trust-on-import warning | Access token JWT `exp` was used without live refresh | Run External IdP Diagnostics after import |
| profile ARN missing | Account can still import but profile will be resolved lazily | Run a live check or first request |
| disabled locally | Kiro-Go will not route requests through that account | Enable local routing in Accounts |
