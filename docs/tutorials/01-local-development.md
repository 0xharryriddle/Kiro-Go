# 01 — Local development

Goal: build Kiro-Go, run it, and reach the admin panel — without any risk to the
real credentials in `data/config.json`.

Every output block below is from a real run on this repo.

## The one thing that can go wrong

`data/config.json` holds **41 real accounts** with live OAuth refresh tokens. The
server's background stats saver writes to whatever `CONFIG_PATH` points at, so a
dev process started with the default path will write to production state.

That is the entire reason `scripts/dev.sh` exists. It builds to a private temp
dir, points `CONFIG_PATH` at a throwaway file, and removes everything on exit.

## Fastest path

```bash
./scripts/dev.sh --smoke
```

`--smoke` starts the server, probes five endpoints, prints the result, and exits
0 or 1 without waiting for input. Real output:

```
starting from a blank config (server will create defaults)
building
  ok   built kiro-go
starting on 127.0.0.1:18080 with CONFIG_PATH=/tmp/kirogo-dev.qC4QK8/config.json
  ok   healthz: {"status":"ok","time":1786179014}

endpoint probes
  ok   /healthz                   200  {"status":"ok","time":1786179014}
  ok   /readyz                    200  {"checks":{"accounts":0,"config":true,"configWritable":true,"importsWritable":true,"webAssets":true},"status":"ok"}
  ok   /v1/models                 200  {"data":[{"capabilities":{"image":true,...
  ok   /admin                     200  <!DOCTYPE html><html lang="zh">...
  ok   /check                     200  <!DOCTYPE html><html lang="vi">...

  ok   smoke passed
stopping server (pid 768807)
```

The isolation is verifiable, not just claimed — the real config is byte-identical
across the run:

```bash
$ sha256sum data/config.json          # before
609f736aea3b8cbb98875e03ab80ccd566c7daa8fd98b5d026e3d032670257bc  data/config.json
$ ./scripts/dev.sh --smoke >/dev/null && sha256sum data/config.json   # after
609f736aea3b8cbb98875e03ab80ccd566c7daa8fd98b5d026e3d032670257bc  data/config.json
```

Do that check yourself the first time you run it. Trusting a script's own claim
about not touching production is how production gets touched.

## Interactive session

Drop `--smoke` to keep the server up:

```bash
./scripts/dev.sh
```

It prints the connection details and blocks until Ctrl-C:

```
  admin panel   http://127.0.0.1:18080/admin
  admin pass    devpassword-not-production
  usage portal  http://127.0.0.1:18080/check
  config        /tmp/kirogo-dev.XXXXXX/config.json   (throwaway — the real one is untouched)
  server log    /tmp/kirogo-dev.XXXXXX/server.log

  Ctrl-C stops the server and removes the temp dir.
```

Ctrl-C sends **SIGTERM**, not SIGKILL, so shutting down this way exercises the
real graceful-shutdown path (`signal.NotifyContext` → `srv.Shutdown` →
`Handler.Close`) rather than bypassing it. If you are debugging shutdown
behaviour, this is the reproduction.

Options:

| Flag | Effect |
|---|---|
| `--port N` | pin the port instead of scanning 18080-18085 |
| `--seed` | start from a **redacted** copy of the real config |
| `--keep` | leave the temp dir in place on exit (prints its path) |
| `--smoke` | probe endpoints, exit 0/1, no interaction |

## `--seed`: realistic shape, no live secrets

A blank config has zero accounts, so anything about routing, model matrices, or
per-key limits is untestable. `--seed` copies the real config and strips it:

```
$ ./scripts/dev.sh --seed --smoke
seeded 41 redacted, disabled account(s)
...
  ok   /readyz   200  {"checks":{"accounts":41,...},"status":"ok"}
```

`accounts:41` in `/readyz` confirms the seed actually loaded. What the redaction
does, measured against the produced file:

- every value under `accessToken`, `refreshToken`, `clientSecret`, `kiroApiKey`,
  `apiKey`, `bedrockSecretAccessKey`, `bedrockSessionToken`,
  `bedrockAccessKeyID`, `idpClientSecret`, `key`, `keyHash` and
  `refreshTokenFingerprint` is blanked;
- every account is set `enabled: false` — a redacted credential cannot serve
  traffic, and leaving accounts enabled would make the pool dispatch and fail on
  every request;
- the admin password is replaced with `devpassword-not-production`.

Verified by cross-checking the two files rather than by reading the code: of the
**98** non-empty secret values in the real config, **0** appear anywhere in the
seeded copy, **0** accounts are enabled, and the account count is preserved
(41 → 41). Re-run that check if you change the redaction list.

`--seed` needs `python3` (it does the redaction). Without it, the script says so
and falls back to a blank config instead of silently copying secrets.

## Running the binary by hand

If you need flags the script does not pass:

```bash
go build -o /tmp/kiro-go .

CONFIG_PATH=/tmp/my-dev-config.json \
ADMIN_PASSWORD=devpassword \
PORT=18080 HOST=127.0.0.1 \
  /tmp/kiro-go
```

Two things to get right:

1. **Always set `CONFIG_PATH`.** Unset, it defaults to the production path.
2. **Run from the repo root.** `/readyz` checks `isPathWritable("data")` and
   `fileExists("web/index.html")` as paths *relative to the process working
   directory* (`proxy/handler.go:4583-4586`). Start the binary from anywhere else
   and `/readyz` reports `degraded` even though the server is fine. This is a
   real trap: the status is accurate about what it measured, just not about what
   you meant.

Precedence for port and host is flag > env > `config.json`:

```bash
./kiro-go -port 9090      # flag wins
PORT=9090 ./kiro-go       # env, if no flag
```

## Environment variables

Measured with `grep -rhoE 'os\.Getenv\("[A-Z_]+"\)' --include='*.go' .` — this is
the complete set the code reads:

| Variable | Purpose |
|---|---|
| `CONFIG_PATH` | config file location — **always set this in dev** |
| `ADMIN_PASSWORD` | overrides the stored admin password for the process lifetime |
| `PORT`, `HOST` | listen address (flags take precedence) |
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error`; default `info` |
| `KIRO_IMPORT_WATCH`, `KIRO_IMPORT_DIR` | credential drop-folder watcher |
| `KIRO_IDE_CACHE`, `KIRO_IDE_PROFILE` | import from the host's Kiro IDE cache |
| `KIRO_PROFILE_REGIONS` | override the profile-probe region list |
| `KIRO_SSO_CALLBACK_BIND` | bind host for the Enterprise-SSO callback listener |
| `TRACE_CAPTURE_MODE`, `TRACE_CAPTURE_ACK_RISK` | request-trace capture |
| `BEDROCK_MODEL_MAP` | Bedrock model alias overrides |

`ADMIN_PASSWORD` is stored separately from the persisted config on purpose, so a
later `Save()` cannot write the env secret into `config.json` in cleartext. One
consequence bites in tests: `config.Init()` does **not** clear it, so a test that
calls `config.SetPassword` leaks into later tests in the same package.

`LOOPBACK_HOST` is **not** read by this tree. The incoming v1.2.8 side used it for
the SSO callback bind, but its Go reader did not survive the merge — only
`KIRO_SSO_CALLBACK_BIND` is read (`auth/kiro_sso.go:299`). Setting `LOOPBACK_HOST`
looks like configuration and does nothing.

## Endpoints worth knowing

```bash
curl -s http://127.0.0.1:18080/healthz   # {"status":"ok","time":1786179014}
curl -s http://127.0.0.1:18080/readyz    # per-check detail, "degraded" if any fail
```

`/healthz` returns only `status` and `time`. It does **not** return `uptime` or
`version` — if you find a doc claiming otherwise, that doc is stale, and this is
`proxy/handler.go:4573-4575`.

Admin API calls need the password header:

```bash
curl -s -H 'X-Admin-Password: devpassword-not-production' \
  http://127.0.0.1:18080/admin/api/accounts
```

Both admin gates share one brute-force throttle keyed on `RemoteAddr`: 5 failures
starts a lockout that doubles from 2s to a 15-minute cap. If admin auth starts
returning 429 during development, you are locked out, not broken — wait it out or
restart the dev server, since the throttle is in-memory.

Customer routes answer without credentials in the default posture:

```bash
curl -s http://127.0.0.1:18080/v1/models
```

That is deliberate (see the tutorial index). A 401 here means someone enabled
`requireApiKey` without minting a key.

## Next

- [02 — The verification gate](02-verification-gate.md): what must pass before you commit.
- [03 — Deploy and rollback](03-deploy-and-rollback.md): shipping it.
