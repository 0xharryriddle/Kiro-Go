# Kiro-Go tutorials

Task-oriented walkthroughs for working on this fork. Every command here was run
against this repo and every output block is copied from a real run — nothing is
illustrative. Where a value drifts (image digests, timestamps, account counts),
the tutorial says how to re-measure it rather than asking you to trust the number.

| # | Tutorial | Answers |
|---|---|---|
| 01 | [Local development](01-local-development.md) | How do I build and run this without touching production credentials? |
| 02 | [The verification gate](02-verification-gate.md) | What must be green before I commit, and why isn't `go test` enough? |
| 03 | [Deploy and rollback](03-deploy-and-rollback.md) | How do I ship a change, prove the image contains it, and undo it? |

The three scripts these document live in `scripts/`:

```
scripts/verify.sh    the full local gate (10 checks)
scripts/dev.sh       run locally against a throwaway config
scripts/deploy.sh    build, verify the image, swap, roll back
```

All three take `--help`. None of them writes `data/config.json`.

## Read this first: `go test` is not the gate

The `hian699` v1.2.8 merge left two artifacts broken while `go build ./...`,
`go vet ./...` and `go test ./...` all stayed green:

- `web/app.js` did not parse (`SyntaxError`), so the **entire** admin bundle
  failed to load and every panel control was dead;
- `docker-compose.yml` was invalid YAML, so `docker compose config` failed and
  the whole deploy path was broken.

Neither is Go code, so no Go tool could see either one. `.github/workflows/ci.yml`
runs build, vet, gofmt and `go test -race` — **Go only** — so CI would also have
passed both. That is why `scripts/verify.sh` exists and why it checks JS, locale
JSON, locale symmetry and Compose validity alongside the Go steps.

Full current state of the gate, from a real run:

```
$ ./scripts/verify.sh
Kiro-Go verify  repo=/home/harry-riddle/dev/github.com/0xharryriddle/Kiro-Go
  go version go1.25.6 linux/amd64

Go
  PASS  build                              go build ./...
  PASS  vet                                go vet ./...
  PASS  gofmt                              gofmt -l . is empty
  PASS  test                               go test ./...
  SKIP  race                               pass --race to include it

Web assets
  PASS  js-parse                           4 file(s) parse
  PASS  locale-json                        3 file(s) valid
  PASS  locale-symmetry                    en == zh (1152 leaf keys)
        vi coverage: 705/1152 keys (partial by design, not gated)

Deploy manifest
  PASS  yaml                               docker-compose.yml parses
  PASS  config                             docker compose config resolves

Tree hygiene
  PASS  whitespace                         git diff --check clean

─────────────────────────────
  passed 10   failed 0   skipped 1
  gate green
```

## Toolchain versions, and why they differ

Three Go versions are in play and the difference is deliberate:

| Where | Version | Source |
|---|---|---|
| `go.mod` | 1.21 | declared language level |
| Dockerfile builder | 1.23 | `golang:1.23-alpine`, `Dockerfile:2` |
| CI | 1.23 | `GO_VERSION: '1.23'`, `ci.yml:31` |
| This machine | 1.25.6 | `go version` |

CI matches the Dockerfile because that is the toolchain which compiles the
shipped binary. Your local Go being newer is fine for development, but a build
that only works on 1.25 will fail CI — if you hit that, reproduce with 1.23
before assuming CI is wrong.

## Conventions these tutorials rely on

- **`data/` is production.** It holds `config.json` with real accounts and live
  OAuth refresh tokens, and it is gitignored (`.gitignore:9-10`). Never point a
  dev process at it; `scripts/dev.sh` handles this for you.
- **The proxy is intentionally unauthenticated.** `requireApiKey` is false with
  zero API keys defined, so customer routes answer 200 with no credential. This
  is an operator decision for an internal proxy, not an oversight. Do **not**
  flip `requireApiKey` on as hardening: with no keys minted it 401s every
  request, which is an outage. Mint a key, update clients, then enable.
- **Zero third-party Go dependencies** beyond `github.com/google/uuid`. Anything
  that needs a new module needs a decision first, not a quiet `go get`.

## Related documentation

- [docs/operator-runbook.md](../operator-runbook.md) — production health checks,
  config backup/restore, credential recovery, diagnostics
- [docs/request-tracing.md](../request-tracing.md) — per-request tracing, capture
  modes, what gets scrubbed
- [docs/kiro-api-key-and-profiles.md](../kiro-api-key-and-profiles.md) — API-key
  credentials, profile/region selection
- [docs/external-idp-import.md](../external-idp-import.md) — Microsoft 365 /
  external IdP import
- [docs/plans/CHECKPOINT_audit_and_merge_state.md](../plans/CHECKPOINT_audit_and_merge_state.md)
  — the authoritative audit record: every defect closed, every claim refuted
