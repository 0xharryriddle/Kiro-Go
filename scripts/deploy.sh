#!/usr/bin/env bash
# deploy.sh — build, verify, and swap the Kiro-Go container, with a rollback tag
# created BEFORE anything is replaced.
#
# WHY THIS EXISTS: four traps have each cost a real debugging session on this
# repo, and every one of them is silent. They are encoded here as checks rather
# than left in a doc nobody re-reads:
#
#   1. Committing and pushing deploys NOTHING. The Dockerfile compiles from
#      source at build time, so the container kept running pre-merge code for
#      ~37 hours once. Only `docker compose build` changes what runs.
#   2. `docker compose images -q` can return a digest the daemon no longer has.
#      `docker save` then silently produces nothing and every symbol check reads
#      ABSENT. Extract from a container created off the TAG instead.
#   3. A symbol check with no positive control cannot tell "missing code" from
#      "broken probe". Every check here greps for a symbol known to exist too.
#   4. Go `const` values are inlined and emit no symbol, so grepping for a const
#      NAME proves nothing. Grep the VALUE.
#
# Usage:
#   scripts/deploy.sh                 # preflight only: gate + build + verify image
#   scripts/deploy.sh --deploy        # preflight, then actually swap the container
#   scripts/deploy.sh --skip-gate     # skip scripts/verify.sh (not recommended)
#   scripts/deploy.sh --rollback TAG  # restore a previously tagged image
#
# Default is deliberately NOT a deploy: it builds and verifies so you can read
# the result, then tells you the exact command to swap. Swapping is the one step
# that touches a live service, so it stays opt-in.

set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2
REPO_ROOT="$PWD"

COMPOSE_SERVICE='kiro-go'
IMAGE_TAG='kiro-go-kiro-go:latest'   # what `docker compose build` produces here
HEALTH_URL="http://127.0.0.1:${KIRO_PORT:-8080}/healthz"

DO_DEPLOY=0
SKIP_GATE=0
ROLLBACK_TAG=''

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy)    DO_DEPLOY=1; shift ;;
    --skip-gate) SKIP_GATE=1; shift ;;
    --rollback)  ROLLBACK_TAG="${2:?--rollback needs an image tag}"; shift 2 ;;
    -h|--help)   sed -n '2,30p' "$0"; exit 0 ;;
    *) printf 'unknown option: %s (try --help)\n' "$1" >&2; exit 2 ;;
  esac
done

if [[ -t 1 ]]; then C_OK=$'\033[32m'; C_NO=$'\033[31m'; C_WARN=$'\033[33m'; C_DIM=$'\033[2m'; C_Z=$'\033[0m'
else C_OK=''; C_NO=''; C_WARN=''; C_DIM=''; C_Z=''; fi
ok()   { printf '  %sok%s    %s\n' "$C_OK" "$C_Z" "$*"; }
bad()  { printf '  %sfail%s  %s\n' "$C_NO" "$C_Z" "$*"; }
warn() { printf '  %swarn%s  %s\n' "$C_WARN" "$C_Z" "$*"; }
dim()  { printf '%s%s%s\n' "$C_DIM" "$*" "$C_Z"; }
step() { printf '\n%s== %s%s\n' "$C_DIM" "$*" "$C_Z"; }

command -v docker >/dev/null 2>&1 || { bad 'docker not on PATH'; exit 2; }

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/kirogo-deploy.XXXXXX")" || exit 2
CREATED_CID=''
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  # Remove the throwaway container this run created, if any. Never a prune.
  if [[ -n "$CREATED_CID" ]]; then docker rm -f "$CREATED_CID" >/dev/null 2>&1; fi
  rm -rf -- "$WORKDIR"
  exit $code
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------- rollback -------
if [[ -n "$ROLLBACK_TAG" ]]; then
  step "rollback to $ROLLBACK_TAG"
  if ! docker image inspect "$ROLLBACK_TAG" >/dev/null 2>&1; then
    bad "image not found locally: $ROLLBACK_TAG"
    dim 'available rollback tags:'
    docker images --filter 'reference=kiro-go-kiro-go:rollback-*' \
      --format '  {{.Repository}}:{{.Tag}}  {{.ID}}  {{.CreatedSince}}' 2>/dev/null
    exit 1
  fi
  ok "found $ROLLBACK_TAG ($(docker image inspect --format '{{.Id}}' "$ROLLBACK_TAG" | cut -c1-19))"
  docker tag "$ROLLBACK_TAG" "$IMAGE_TAG" || { bad 'retag failed'; exit 1; }
  ok "retagged as $IMAGE_TAG"
  # --no-build is load-bearing: rebuilding would recompile current source and
  # defeat the point of restoring the exact prior artifact.
  docker compose up -d --no-build "$COMPOSE_SERVICE" || { bad 'compose up failed'; exit 1; }
  ok 'container recreated from the rollback image'
  printf '\n  verify: curl -s %s\n' "$HEALTH_URL"
  exit 0
fi

# ------------------------------------------------------------- 1. gate --------
if [[ $SKIP_GATE -eq 1 ]]; then
  warn 'gate skipped (--skip-gate): shipping unverified source'
else
  step 'gate (scripts/verify.sh)'
  if [[ ! -x scripts/verify.sh ]]; then
    bad 'scripts/verify.sh missing or not executable'; exit 1
  fi
  if ./scripts/verify.sh >"$WORKDIR/gate.txt" 2>&1; then
    ok "$(tail -2 "$WORKDIR/gate.txt" | head -1 | tr -s ' ')"
  else
    bad 'gate failed — refusing to build'
    sed 's/^/      /' "$WORKDIR/gate.txt" | tail -30
    exit 1
  fi
fi

# --------------------------------------------------- 2. state + rollback tag --
step 'current state'
RUNNING_ID="$(docker ps --filter "name=${COMPOSE_SERVICE}" --format '{{.ID}}' | head -1)"
if [[ -n "$RUNNING_ID" ]]; then
  ok "container running: $RUNNING_ID"
else
  warn 'no running container (first deploy, or it is stopped)'
fi

# Tag BEFORE building: `docker compose build` moves :latest, and without a prior
# tag the old image becomes dangling and unreachable by name.
ROLLBACK_CREATED=''
if docker image inspect "$IMAGE_TAG" >/dev/null 2>&1; then
  CUR_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE_TAG")"
  SHORT="$(printf '%s' "$CUR_ID" | sed 's/^sha256://' | cut -c1-8)"
  ROLLBACK_CREATED="kiro-go-kiro-go:rollback-$SHORT"
  if docker image inspect "$ROLLBACK_CREATED" >/dev/null 2>&1; then
    ok "rollback tag already exists: $ROLLBACK_CREATED"
  else
    docker tag "$IMAGE_TAG" "$ROLLBACK_CREATED" && ok "tagged rollback: $ROLLBACK_CREATED"
  fi
else
  warn "no existing $IMAGE_TAG to tag — nothing to roll back to"
fi

# ------------------------------------------------------------ 3. build --------
step 'build'
dim 'the Dockerfile compiles from source: this is the ONLY step that changes what runs'
if docker compose build "$COMPOSE_SERVICE" >"$WORKDIR/build.txt" 2>&1; then
  ok 'image built'
else
  bad 'docker compose build failed'
  sed 's/^/      /' "$WORKDIR/build.txt" | tail -30
  exit 1
fi

# -------------------------------------------------- 4. verify image by TAG ----
step 'verify image contents'
# Trap 2: create from the TAG, never trust `compose images -q` digests.
CREATED_CID="$(docker create "$IMAGE_TAG" 2>/dev/null)"
if [[ -z "$CREATED_CID" ]]; then
  bad "could not create a container from $IMAGE_TAG"; exit 1
fi
if docker cp "$CREATED_CID:/app/kiro-go" "$WORKDIR/kiro-go" >/dev/null 2>&1; then
  ok "extracted binary: $(du -h "$WORKDIR/kiro-go" | cut -f1), sha256 $(sha256sum "$WORKDIR/kiro-go" | cut -c1-16)"
else
  bad 'could not extract /app/kiro-go from the image'; exit 1
fi
docker rm -f "$CREATED_CID" >/dev/null 2>&1; CREATED_CID=''

# Trap 3 + 4: a positive control proves the probe works, and the version is
# checked by VALUE because `const Version` is inlined and emits no symbol.
# Materialize `strings` output ONCE to a file, then grep the file.
#
# This is not a style choice. `strings BIN | grep -q PAT` under `set -o pipefail`
# exits 141, not 0: grep -q stops reading at the first match, closing the pipe,
# which kills `strings` with SIGPIPE, and pipefail then reports the pipeline as
# failed. Measured on this exact binary — the symbol was present three times and
# the check still said MISSING. Found because the positive control below fired,
# which is precisely why the control exists: it distinguishes a broken probe from
# a bad image, and here the probe was the broken half.
SYMS="$WORKDIR/strings.txt"
if ! strings -a "$WORKDIR/kiro-go" >"$SYMS" 2>/dev/null; then
  bad 'could not run strings on the extracted binary'; exit 1
fi
ok "symbol table dumped: $(wc -l <"$SYMS" | tr -d ' ') strings"

CONTROL_SYMBOL='listKiroProfilesInRegion'
if grep -qF "$CONTROL_SYMBOL" "$SYMS"; then
  ok "positive control present: $CONTROL_SYMBOL"
else
  bad "positive control MISSING ($CONTROL_SYMBOL) — the probe itself is broken, not the image"
  exit 1
fi
# Trap 4: check the VALUE, never the const NAME. `const Version` is inlined by the
# compiler and emits no symbol, so grepping for "Version" proves nothing.
WANT_VERSION="$(grep -oE '^const Version = "[^"]+"' config/config.go | sed 's/.*"\(.*\)"/\1/')"
if [[ -n "$WANT_VERSION" ]] && grep -qF "$WANT_VERSION" "$SYMS"; then
  ok "version value present: $WANT_VERSION"
else
  warn "version value ${WANT_VERSION:-<unreadable>} not found in the binary"
fi

# ------------------------------------------------------------ 5. deploy -------
if [[ $DO_DEPLOY -eq 0 ]]; then
  cat <<EOF

$(dim '─────────────────────────────')
  Built and verified. NOT deployed (default is preflight only).

  swap now:   scripts/deploy.sh --deploy
$( [[ -n "$ROLLBACK_CREATED" ]] && printf '  rollback:   scripts/deploy.sh --rollback %s\n' "$ROLLBACK_CREATED" )
EOF
  exit 0
fi

step 'deploy'
if docker compose up -d --no-build "$COMPOSE_SERVICE" >"$WORKDIR/up.txt" 2>&1; then
  ok 'container recreated'
else
  bad 'docker compose up failed'
  sed 's/^/      /' "$WORKDIR/up.txt" | tail -20
  [[ -n "$ROLLBACK_CREATED" ]] && printf '\n  roll back with: scripts/deploy.sh --rollback %s\n' "$ROLLBACK_CREATED"
  exit 1
fi

step 'health'
# Poll rather than sleep. compose sets start_period: 10s, so allow well past it.
HEALTHY=0
for _ in $(seq 1 60); do
  if curl -fsS --max-time 2 "$HEALTH_URL" >"$WORKDIR/health.json" 2>/dev/null; then HEALTHY=1; break; fi
  sleep 1
done
if [[ $HEALTHY -eq 1 ]]; then
  ok "healthz: $(cat "$WORKDIR/health.json")"
else
  bad "no healthy response from $HEALTH_URL after 60s"
  docker compose logs --tail 30 "$COMPOSE_SERVICE" 2>&1 | sed 's/^/      /'
  [[ -n "$ROLLBACK_CREATED" ]] && printf '\n  roll back with: scripts/deploy.sh --rollback %s\n' "$ROLLBACK_CREATED"
  exit 1
fi

# Docker's own verdict, which lags the endpoint by up to one interval (30s).
STATE="$(docker inspect --format '{{.State.Health.Status}}' "$(docker ps --filter "name=${COMPOSE_SERVICE}" --format '{{.ID}}' | head -1)" 2>/dev/null || echo unknown)"
RESTARTS="$(docker inspect --format '{{.RestartCount}}' "$(docker ps --filter "name=${COMPOSE_SERVICE}" --format '{{.ID}}' | head -1)" 2>/dev/null || echo '?')"
ok "docker health=$STATE restarts=$RESTARTS"

cat <<EOF

$(dim '─────────────────────────────')
  deployed$( [[ -n "$ROLLBACK_CREATED" ]] && printf '\n  rollback available: scripts/deploy.sh --rollback %s' "$ROLLBACK_CREATED" )
EOF
