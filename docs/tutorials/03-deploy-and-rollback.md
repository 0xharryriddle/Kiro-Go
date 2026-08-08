# 03 — Deploy and rollback

Goal: ship a change, prove the running container actually contains it, and undo
it fast if it does not.

Every output block below is from a real run on this repo.

## The trap that matters most

**Committing and pushing deploys nothing.**

The Dockerfile compiles from source at build time (`Dockerfile:15`,
`go build -o kiro-go .`), so the image is a snapshot of source as of the build.
Nothing in a `git commit` or `git push` reaches the running container. This is
not hypothetical: the container on this machine once served pre-merge code for
about **37 hours** after the fix was committed and pushed, and every symptom
pointed at the code rather than at the deploy.

Only `docker compose build` changes what runs. `scripts/deploy.sh` exists so this
is impossible to forget.

## Preflight: build and verify without touching the live service

```bash
./scripts/deploy.sh
```

The default is **not** a deploy. It runs the gate, tags a rollback, builds, and
verifies the image contents — then stops and tells you the command to swap. Real
output:

```
== gate (scripts/verify.sh)
  ok     passed 10 failed 0 skipped 1

== current state
  warn  no running container (first deploy, or it is stopped)
  ok    tagged rollback: kiro-go-kiro-go:rollback-8197e45d

== build
the Dockerfile compiles from source: this is the ONLY step that changes what runs
  ok    image built

== verify image contents
  ok    extracted binary: 13M, sha256 9f34efb0c4a9ba76
  ok    symbol table dumped: 151394 strings
  ok    positive control present: listKiroProfilesInRegion
  ok    version value present: 1.2.8

─────────────────────────────
  Built and verified. NOT deployed (default is preflight only).

  swap now:   scripts/deploy.sh --deploy
  rollback:   scripts/deploy.sh --rollback kiro-go-kiro-go:rollback-8197e45d
```

| Flag | Effect |
|---|---|
| *(none)* | gate + rollback tag + build + verify image, then stop |
| `--deploy` | all of the above, then swap the container and poll health |
| `--skip-gate` | skip `verify.sh` — ships unverified source, prints a warning |
| `--rollback TAG` | retag `TAG` as `:latest` and recreate the container |

The health probe uses `http://127.0.0.1:${KIRO_PORT:-8080}/healthz`. Set
`KIRO_PORT` if the published port differs.

## Ordering: the rollback tag is created before the build

`docker compose build` moves the `:latest` tag to the new image. If nothing
tagged the previous image first, it becomes dangling — still on disk, but
unreachable by name, exactly when you need it. So the script tags first:

```
== current state
  ok    tagged rollback: kiro-go-kiro-go:rollback-8197e45d
```

The tag suffix is the first 8 hex of the image ID, so it identifies the artifact
rather than a timestamp or a guess at which commit produced it.

One consequence to know: **a failed preflight can still have built.** If the run
dies after the build step, `:latest` has already moved. Check what tags exist
before assuming your rollback target is intact:

```bash
$ docker images --filter 'reference=kiro-go-kiro-go*' \
    --format '{{.Repository}}:{{.Tag}} {{.ID}} {{.CreatedSince}}'
kiro-go-kiro-go:latest             95787fb4a9e8  3 minutes ago
kiro-go-kiro-go:rollback-8197e45d  8197e45d6c86  3 minutes ago
kiro-go-kiro-go:rollback-2958ed77  2958ed773ea8  9 days ago
kiro-go-kiro-go:rollback-71f4e867  71f4e867f174  11 days ago
```

The 9- and 11-day-old tags are prior production images, still reachable by name.
That is the property to preserve — never `docker image prune` in this repo
without reading this list first.

## Verifying the image really contains your change

This is the step that would have caught the 37-hour incident. Three traps make
naive verification lie, and each is encoded as a check:

**Trap: `docker compose images -q` can return a digest the daemon no longer
has.** `docker save` then silently writes nothing and every symbol check reads
ABSENT — pointing at your code when the real fault was the extraction. The script
creates a container from the **tag** instead:

```bash
CID=$(docker create kiro-go-kiro-go:latest)
docker cp "$CID:/app/kiro-go" ./kiro-go
docker rm -f "$CID"
```

**Trap: a symbol check without a positive control cannot distinguish "missing
code" from "broken probe".** So the script also greps for a symbol known to
exist. This earned its place immediately — on the first real run it fired:

```
  fail  positive control MISSING (listKiroProfilesInRegion) — the probe itself is broken, not the image
```

The symbol *was* in the image (3 occurrences, confirmed via `go tool nm`). The bug
was in the probe: under `set -o pipefail`,

```bash
strings BIN | grep -q PATTERN     # exits 141, not 0
```

`grep -q` stops reading at the first match, closing the pipe, which kills
`strings` with SIGPIPE; `pipefail` then reports the whole pipeline as failed. The
fix is to materialize `strings` output to a file once and grep the file. Measured:

```
with pipefail    rc=141
without pipefail rc=0
grep -c          rc=0 count=3
```

Without the positive control this would have read as "the image is missing my
code" and sent someone debugging the merge instead of the script.

**Trap: Go `const` values are inlined and emit no symbol.** Grepping for the
const *name* `Version` proves nothing. Grep the *value*:

```
  ok    version value present: 1.2.8
```

To check by hand for a symbol you care about:

```bash
CID=$(docker create kiro-go-kiro-go:latest)
docker cp "$CID:/app/kiro-go" /tmp/kiro-go && docker rm -f "$CID"
strings -a /tmp/kiro-go > /tmp/syms.txt
grep -cF 'yourNewFunctionName' /tmp/syms.txt   # your change
grep -cF 'listKiroProfilesInRegion' /tmp/syms.txt   # positive control: must be > 0
```

If the control returns 0, stop — your extraction is broken, not the image.

## Deploying

```bash
./scripts/deploy.sh --deploy
```

After swapping it polls `/healthz` once a second for up to 60s, then reports
Docker's own verdict:

```
== deploy
  ok    container recreated

== health
  ok    healthz: {"status":"ok","time":...}
  ok    docker health=healthy restarts=0
```

Two details:

- `docker compose up -d --no-build` is used for the swap. The image was already
  built and verified; rebuilding here would recompile source that may have
  changed since verification and ship something that was never checked.
- **Endpoint health and Docker health disagree for up to 30s.** The compose
  healthcheck has `interval: 30s` and `start_period: 10s`, so a fresh container
  can answer `/healthz` correctly while Docker still says `starting`. Both are
  reported because either alone is misleading. `restarts=0` is the number to
  watch — a nonzero count means it is crash-looping regardless of the current
  status.

Current state on this machine, for reference:

```
$ docker ps -a --filter 'name=kiro-go' --format '{{.Names}} {{.Status}}'
kiro-go-kiro-go-1 Exited (0) 19 hours ago
```

Exit code 0 means a clean stop, not a crash — consistent with the graceful
shutdown path.

## Rolling back

```bash
./scripts/deploy.sh --rollback kiro-go-kiro-go:rollback-8197e45d
```

It retags that image as `:latest` and recreates the container with `--no-build`.
That flag is load-bearing: rebuilding would recompile current source and defeat
the entire point of restoring the prior artifact.

Pass a tag that does not exist and it lists the ones that do rather than failing
blind:

```
  fail  image not found locally: kiro-go-kiro-go:rollback-nope
available rollback tags:
  kiro-go-kiro-go:rollback-8197e45d  8197e45d6c86  3 minutes ago
  kiro-go-kiro-go:rollback-2958ed77  2958ed773ea8  9 days ago
```

Rollback restores the **binary**, not `data/config.json`. Config changes made by
the newer version persist across a rollback, so if a release migrated config, the
older binary will read the migrated file. Back up config separately — see
[docs/operator-runbook.md](../operator-runbook.md).

## After deploying

Confirm the container is running the artifact you verified, not something else:

```bash
docker inspect --format '{{.Image}}' \
  "$(docker ps --filter name=kiro-go --format '{{.ID}}')"
docker image inspect --format '{{.Id}}' kiro-go-kiro-go:latest
```

Those two must match. If they differ, the container predates the build and the
swap did not happen.

## What deploy.sh does not do

- **No `docker image prune`, ever.** Cleanup by glob or prune is how rollback
  targets vanish. It removes only the specific throwaway container it created,
  by ID.
- **No `data/` writes.** It never reads or modifies production config.
- **No push to a registry.** Local image only.
- **No smoke test beyond `/healthz`.** A healthy process is not a working proxy;
  it does not send a real completion request upstream.

## Next

- [docs/operator-runbook.md](../operator-runbook.md) — production health checks,
  config backup/restore, credential recovery, diagnostics
- [01 — Local development](01-local-development.md)
- [02 — The verification gate](02-verification-gate.md)
