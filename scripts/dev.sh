#!/usr/bin/env bash
# dev.sh — run Kiro-Go locally against a THROWAWAY config, never the real one.
#
# WHY THIS EXISTS: data/config.json holds real accounts and live OAuth refresh
# tokens. Starting the server with the default CONFIG_PATH points a dev process
# at that file, and the background stats saver WILL write to it. This script
# always runs against an isolated copy under a private temp dir, so a dev run
# cannot corrupt, re-order, or partially flush production credentials.
#
# Usage:
#   scripts/dev.sh                    # blank config, random-ish free port
#   scripts/dev.sh --port 18080       # pin the port
#   scripts/dev.sh --seed             # start from a REDACTED copy of the real
#                                     # config (accounts stripped of secrets)
#   scripts/dev.sh --keep             # do not delete the temp dir on exit
#   scripts/dev.sh --smoke            # start, probe endpoints, print, exit 0/1
#
# Ctrl-C stops the server and removes the temp dir. --smoke needs no keyboard,
# so it is what CI or a quick sanity check should call.

set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2
REPO_ROOT="$PWD"

PORT=""
SEED=0
KEEP=0
SMOKE=0
ADMIN_PW="devpassword-not-production"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port)  PORT="${2:?--port needs a value}"; shift 2 ;;
    --seed)  SEED=1; shift ;;
    --keep)  KEEP=1; shift ;;
    --smoke) SMOKE=1; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) printf 'unknown option: %s (try --help)\n' "$1" >&2; exit 2 ;;
  esac
done

if [[ -t 1 ]]; then C_OK=$'\033[32m'; C_NO=$'\033[31m'; C_DIM=$'\033[2m'; C_Z=$'\033[0m'
else C_OK=''; C_NO=''; C_DIM=''; C_Z=''; fi
say()  { printf '%s\n' "$*"; }
dim()  { printf '%s%s%s\n' "$C_DIM" "$*" "$C_Z"; }
ok()   { printf '  %sok%s   %s\n' "$C_OK" "$C_Z" "$*"; }
bad()  { printf '  %sfail%s %s\n' "$C_NO" "$C_Z" "$*"; }

command -v go >/dev/null 2>&1 || { bad 'go not on PATH'; exit 2; }

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/kirogo-dev.XXXXXX")" || exit 2
BIN="$WORKDIR/kiro-go"
DEV_CONFIG="$WORKDIR/config.json"
SRV_PID=""
SRV_LOG="$WORKDIR/server.log"

# Cleanup must be idempotent and must not outlive the child. It runs on normal
# exit, Ctrl-C, and SIGTERM. Only the directory this run created is removed —
# never a glob or a prefix sweep.
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  if [[ -n "$SRV_PID" ]] && kill -0 "$SRV_PID" 2>/dev/null; then
    dim "stopping server (pid $SRV_PID)"
    # SIGTERM first: main.go installs signal.NotifyContext, so this exercises the
    # real graceful-shutdown path (drain + Handler.Close) rather than SIGKILL.
    kill -TERM "$SRV_PID" 2>/dev/null
    for _ in $(seq 1 50); do
      kill -0 "$SRV_PID" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$SRV_PID" 2>/dev/null; then
      dim 'server ignored SIGTERM after 5s; sending SIGKILL'
      kill -KILL "$SRV_PID" 2>/dev/null
      wait "$SRV_PID" 2>/dev/null
    fi
  fi
  if [[ $KEEP -eq 1 ]]; then
    dim "kept: $WORKDIR"
  else
    rm -rf -- "$WORKDIR"
  fi
  exit $code
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------- port --------
port_free() {
  # No `ss`/`lsof` dependency: bash's own /dev/tcp probe is enough and portable
  # here. A refused connection means nothing is listening.
  ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}
if [[ -z "$PORT" ]]; then
  for candidate in 18080 18081 18082 18083 18084 18085; do
    if port_free "$candidate"; then PORT="$candidate"; break; fi
  done
  [[ -n "$PORT" ]] || { bad 'no free port in 18080-18085; pass --port'; exit 2; }
elif ! port_free "$PORT"; then
  bad "port $PORT is already in use"; exit 2
fi

# -------------------------------------------------------------- config --------
if [[ $SEED -eq 1 && -f data/config.json ]]; then
  # Redact before use. The dev process must never hold a live refresh token, and
  # a redacted seed is still useful: it reproduces account COUNT, model routing,
  # and per-key limits without carrying anything that can authenticate upstream.
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$REPO_ROOT/data/config.json" "$DEV_CONFIG" <<'PY'
import json, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src, encoding='utf-8') as fh:
    cfg = json.load(fh)
SECRET_KEYS = (
    'accessToken', 'refreshToken', 'clientSecret', 'kiroApiKey', 'apiKey',
    'bedrockSecretAccessKey', 'bedrockSessionToken', 'bedrockAccessKeyID',
    'idpClientSecret', 'key', 'keyHash',
    # Not a credential (it is a one-way hash used to reject a re-imported token),
    # but it carries zero dev-time value once the tokens themselves are blank, and
    # keeping it would let someone confirm whether a token they already hold is in
    # this fleet. Cheap to drop, so drop it.
    'refreshTokenFingerprint',
)
def scrub(node):
    if isinstance(node, dict):
        return {k: ('' if k in SECRET_KEYS and isinstance(v, str) else scrub(v))
                for k, v in node.items()}
    if isinstance(node, list):
        return [scrub(v) for v in node]
    return node
cfg = scrub(cfg)
# Disable every account: a redacted credential cannot serve traffic, and leaving
# them enabled would make the pool dispatch and fail on every request.
for acc in cfg.get('accounts', []):
    acc['enabled'] = False
cfg['password'] = 'devpassword-not-production'
with open(dst, 'w', encoding='utf-8') as fh:
    json.dump(cfg, fh, indent=2)
print(f"seeded {len(cfg.get('accounts', []))} redacted, disabled account(s)")
PY
  else
    bad 'python3 needed for --seed (it does the redaction); starting blank instead'
    SEED=0
  fi
fi
if [[ ! -f "$DEV_CONFIG" ]]; then
  # A missing CONFIG_PATH is fine: config.Load() creates defaults and saves.
  dim 'starting from a blank config (server will create defaults)'
fi

# --------------------------------------------------------------- build --------
dim 'building'
if ! go build -o "$BIN" . >"$WORKDIR/build.log" 2>&1; then
  bad 'go build failed'; sed 's/^/    /' "$WORKDIR/build.log" | head -20; exit 1
fi
ok "built $(basename "$BIN")"

# --------------------------------------------------------------- start --------
# CWD matters: handleReadyz checks isPathWritable("data") and
# fileExists("web/index.html") as paths RELATIVE to the process working
# directory (proxy/handler.go:4583-4586). Running from anywhere but the repo root
# makes /readyz report "degraded" even though the server is fine. So the server
# is started from REPO_ROOT with only CONFIG_PATH pointed elsewhere.
dim "starting on 127.0.0.1:$PORT with CONFIG_PATH=$DEV_CONFIG"
CONFIG_PATH="$DEV_CONFIG" \
ADMIN_PASSWORD="$ADMIN_PW" \
PORT="$PORT" \
HOST="127.0.0.1" \
LOG_LEVEL="${LOG_LEVEL:-info}" \
  "$BIN" >"$SRV_LOG" 2>&1 &
SRV_PID=$!

# Readiness by polling, never a blind sleep.
READY=0
for _ in $(seq 1 100); do
  if ! kill -0 "$SRV_PID" 2>/dev/null; then
    bad 'server exited during startup'; sed 's/^/    /' "$SRV_LOG" | tail -20; exit 1
  fi
  if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >"$WORKDIR/healthz.json" 2>/dev/null; then
    READY=1; break
  fi
  sleep 0.1
done
[[ $READY -eq 1 ]] || { bad 'server did not become healthy in 10s'; sed 's/^/    /' "$SRV_LOG" | tail -20; exit 1; }
ok "healthz: $(cat "$WORKDIR/healthz.json")"

# ------------------------------------------------------------- endpoints ------
probe() {
  local path="$1" expect="$2" body code
  code="$(curl -s -o "$WORKDIR/body.txt" -w '%{http_code}' --max-time 5 "http://127.0.0.1:$PORT$path" 2>/dev/null)"
  body="$(head -c 160 "$WORKDIR/body.txt" | tr -d '\n')"
  if [[ "$code" == "$expect" ]]; then ok "$(printf '%-26s %s  %s' "$path" "$code" "$body")"; return 0
  else bad "$(printf '%-26s %s (want %s)  %s' "$path" "$code" "$expect" "$body")"; return 1; fi
}

say ''
dim 'endpoint probes'
FAILED=0
probe /healthz 200 || FAILED=1
probe /readyz  200 || FAILED=1
# /v1/models is a customer route. With zero API keys defined, authenticate()
# returns (nil, nil) — the documented open-by-design posture — so 200 is correct
# here and a 401 would mean someone enabled requireApiKey without minting a key.
probe /v1/models 200 || FAILED=1
probe /admin 200 || FAILED=1
probe /check 200 || FAILED=1

say ''
if [[ $SMOKE -eq 1 ]]; then
  if [[ $FAILED -eq 0 ]]; then ok 'smoke passed'; exit 0; else bad 'smoke failed'; exit 1; fi
fi

cat <<EOF
$(dim '─────────────────────────────')
  admin panel   http://127.0.0.1:$PORT/admin
  admin pass    $ADMIN_PW
  usage portal  http://127.0.0.1:$PORT/check
  config        $DEV_CONFIG   $(dim '(throwaway — the real one is untouched)')
  server log    $SRV_LOG

  Ctrl-C stops the server and removes the temp dir.
EOF

# Wait on the child so Ctrl-C reaches the trap rather than orphaning the server.
wait "$SRV_PID"
