#!/usr/bin/env bash
# verify.sh — the full local gate for Kiro-Go.
#
# WHY THIS EXISTS: `go build && go test` is NOT the gate. The v1.2.8 merge left
# web/app.js unparseable (every admin panel control dead) and docker-compose.yml
# invalid YAML (the whole deploy path broken) while both of those Go commands
# stayed green. Anything that can silently break must be checked here, not
# remembered.
#
# Usage:
#   scripts/verify.sh              # everything except -race (fast path, ~1 min)
#   scripts/verify.sh --race       # everything, including the race detector
#   scripts/verify.sh --go         # Go checks only
#   scripts/verify.sh --web        # web/JS + locale checks only
#   scripts/verify.sh --compose    # docker-compose validity only
#   scripts/verify.sh --list       # print the checks and exit
#
# Exit code is 0 only when every selected check passed. Each check prints
# PASS / FAIL / SKIP with a one-line reason, so a failure names itself.
#
# SAFETY: read-only. It never writes data/config.json (41 real accounts live
# there), never starts a server, and never touches Docker state — `compose
# config` only parses. The only writes are to a private mktemp -d, removed on
# exit by the trap below.

set -uo pipefail
# NOTE: deliberately NOT `set -e`. Every check must run so one failure does not
# hide the rest; failures are accumulated and reported together at the end.

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2
REPO_ROOT="$PWD"

TMPDIR_OWNED="$(mktemp -d "${TMPDIR:-/tmp}/kirogo-verify.XXXXXX")" || exit 2
# Remove ONLY the directory this run created. Never a glob, never a prefix sweep.
cleanup() { rm -rf -- "$TMPDIR_OWNED"; }
trap cleanup EXIT INT TERM

PASS_N=0
FAIL_N=0
SKIP_N=0
FAILED_NAMES=()

if [[ -t 1 ]]; then
  C_OK=$'\033[32m'; C_NO=$'\033[31m'; C_SK=$'\033[33m'; C_DIM=$'\033[2m'; C_Z=$'\033[0m'
else
  C_OK=''; C_NO=''; C_SK=''; C_DIM=''; C_Z=''
fi

pass() { PASS_N=$((PASS_N + 1)); printf '  %sPASS%s  %-34s %s\n' "$C_OK" "$C_Z" "$1" "${2-}"; }
fail() {
  FAIL_N=$((FAIL_N + 1)); FAILED_NAMES+=("$1")
  printf '  %sFAIL%s  %-34s %s\n' "$C_NO" "$C_Z" "$1" "${2-}"
  # Show captured output indented, so the reason is on screen and not in a file
  # the reader has to go find.
  if [[ -n "${3-}" && -s "$3" ]]; then sed 's/^/          /' "$3" | head -25; fi
}
skip() { SKIP_N=$((SKIP_N + 1)); printf '  %sSKIP%s  %-34s %s\n' "$C_SK" "$C_Z" "$1" "${2-}"; }
section() { printf '\n%s%s%s\n' "$C_DIM" "$1" "$C_Z"; }

have() { command -v "$1" >/dev/null 2>&1; }

RUN_GO=0; RUN_WEB=0; RUN_COMPOSE=0; RUN_GIT=0; WITH_RACE=0
SELECTED=0
for arg in "$@"; do
  case "$arg" in
    --go)      RUN_GO=1;      SELECTED=1 ;;
    --web)     RUN_WEB=1;     SELECTED=1 ;;
    --compose) RUN_COMPOSE=1; SELECTED=1 ;;
    --git)     RUN_GIT=1;     SELECTED=1 ;;
    --race)    WITH_RACE=1 ;;
    --list)
      cat <<'EOF'
Checks, in run order:

 go      build            go build ./...
 go      vet              go vet ./...
 go      gofmt            gofmt -l . must be empty
 go      test             go test ./... -count=1
 go      race             go test ./... -race -count=1        (only with --race)
 web     js-parse         node --check on every tracked web/**/*.js
 web     html-structure   every web/*.html: balanced tags, no nested .tab-content
 web     html-ids         every web/*.html: no duplicated id attribute
 web     locale-json      every web/locales/*.json parses
 web     locale-symmetry  en.json and zh.json leaf-key sets must match exactly
 compose yaml             docker-compose.yml parses as YAML
 compose config           docker compose config resolves        (needs docker)
 git     whitespace       git diff --check finds no bad whitespace
EOF
      exit 0 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) printf 'unknown option: %s (try --help)\n' "$arg" >&2; exit 2 ;;
  esac
done
if [[ $SELECTED -eq 0 ]]; then RUN_GO=1; RUN_WEB=1; RUN_COMPOSE=1; RUN_GIT=1; fi

printf '%sKiro-Go verify%s  repo=%s\n' "$C_DIM" "$C_Z" "$REPO_ROOT"
if have go; then printf '%s  %s%s\n' "$C_DIM" "$(go version)" "$C_Z"; fi

# ---------------------------------------------------------------- Go ----------
if [[ $RUN_GO -eq 1 ]]; then
  section 'Go'
  if ! have go; then
    skip 'build/vet/gofmt/test' 'go not on PATH'
  else
    out="$TMPDIR_OWNED/go-build.txt"
    if go build ./... >"$out" 2>&1; then pass 'build' 'go build ./...'
    else fail 'build' 'go build ./... failed' "$out"; fi

    out="$TMPDIR_OWNED/go-vet.txt"
    if go vet ./... >"$out" 2>&1; then pass 'vet' 'go vet ./...'
    else fail 'vet' 'go vet ./... reported problems' "$out"; fi

    out="$TMPDIR_OWNED/gofmt.txt"
    # `gofmt -l` exits 0 even when it lists files, so the emptiness of stdout IS
    # the assertion — not the exit code.
    gofmt -l . >"$out" 2>&1
    if [[ -s "$out" ]]; then fail 'gofmt' "$(wc -l <"$out" | tr -d ' ') file(s) need gofmt -w" "$out"
    else pass 'gofmt' 'gofmt -l . is empty'; fi

    out="$TMPDIR_OWNED/go-test.txt"
    if go test ./... -count=1 >"$out" 2>&1; then
      pass 'test' "$(grep -oE '[0-9]+ (passed|ok)' "$out" | tail -1 || echo 'go test ./...')"
    else
      fail 'test' 'go test ./... failed' "$out"
    fi

    if [[ $WITH_RACE -eq 1 ]]; then
      out="$TMPDIR_OWNED/go-race.txt"
      printf '        %s(race detector: this takes minutes)%s\n' "$C_DIM" "$C_Z"
      if go test ./... -race -count=1 >"$out" 2>&1; then pass 'race' 'no data races'
      else fail 'race' 'go test -race failed' "$out"; fi
    else
      skip 'race' 'pass --race to include it'
    fi
  fi
fi

# --------------------------------------------------------------- web ----------
if [[ $RUN_WEB -eq 1 ]]; then
  section 'Web assets'
  if ! have node; then
    skip 'js-parse' 'node not on PATH'
  else
    out="$TMPDIR_OWNED/js.txt"
    : >"$out"
    js_bad=0 js_n=0
    # Vendored bundles are included on purpose: a corrupt vendor file breaks the
    # panel exactly like a corrupt app.js does.
    while IFS= read -r f; do
      js_n=$((js_n + 1))
      if ! node --check "$f" >>"$out" 2>&1; then js_bad=$((js_bad + 1)); printf '  ^ in %s\n' "$f" >>"$out"; fi
    done < <(find web -name '*.js' -not -path '*/node_modules/*' | sort)
    if [[ $js_bad -eq 0 ]]; then pass 'js-parse' "$js_n file(s) parse"
    else fail 'js-parse' "$js_bad of $js_n file(s) failed to parse" "$out"; fi
  fi

  # HTML structure + id uniqueness.
  #
  # These exist because the gate DEMONSTRABLY missed a real defect without them.
  # The v1.2.8 merge left <div id="tabApilog"> and <div id="tabConsole"> nested
  # INSIDE the hidden <div id="tabLogs">, so both admin tabs could never render —
  # a hidden parent hides its children whatever their own class says. It also left
  # 9 duplicated ids, so getElementById resolved to whichever came first and two
  # different handlers bound the same node. Both merge parents were structurally
  # clean; only the merged result was broken, and go build/vet/test plus
  # `node --check` were all green on it. Nothing here parses HTML but this.
  if ! have python3; then
    skip 'html-structure/html-ids' 'python3 not on PATH'
  else
    HTML_PROBE="$TMPDIR_OWNED/html_probe.py"
    cat >"$HTML_PROBE" <<'PY'
import sys, glob, re, collections
from html.parser import HTMLParser

VOID = {'area','base','br','col','embed','hr','img','input','link','meta',
        'param','source','track','wbr'}
MODE = sys.argv[1]

class Checker(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.stack = []      # [(tag, line, is_tab_content)]
        self.errors = []
        self.ids = collections.defaultdict(list)
    def handle_starttag(self, tag, attrs):
        d = dict(attrs)
        # Collect ids HERE, from parsed elements only. A regex over raw text
        # would also match id="..." inside <script> bodies, where two branches
        # of an if/else legitimately build the same id and only one ever runs —
        # measured: web/index-legacy.html:2411/2415. HTMLParser hands script
        # content to handle_data as CDATA, so element ids are the only ones seen.
        if d.get('id'):
            self.ids[d['id']].append(self.getpos()[0])
        if tag in VOID:
            return
        cls = d.get('class') or ''
        is_tab = 'tab-content' in cls.split()
        if is_tab:
            for t, ln, was_tab in self.stack:
                if was_tab:
                    self.errors.append(
                        'line %d: <%s class="tab-content"> nested inside the one '
                        'opened at line %d — a hidden parent hides this tab forever'
                        % (self.getpos()[0], tag, ln))
                    break
        self.stack.append((tag, self.getpos()[0], is_tab))
    def handle_endtag(self, tag):
        if tag in VOID:
            return
        for i in range(len(self.stack) - 1, -1, -1):
            if self.stack[i][0] == tag:
                unclosed = self.stack[i+1:]
                if unclosed:
                    self.errors.append(
                        'line %d: </%s> closed while still open: %s'
                        % (self.getpos()[0], tag,
                           ', '.join('<%s>@%d' % (t, ln) for t, ln, _ in unclosed)))
                del self.stack[i:]
                return
        self.errors.append('line %d: stray </%s>' % (self.getpos()[0], tag))

bad = 0
n = 0
for path in sorted(glob.glob('web/*.html')):
    n += 1
    text = open(path, encoding='utf-8').read()
    if MODE == 'structure':
        c = Checker()
        c.feed(text)
        errs = list(c.errors)
        if c.stack:
            errs.append('unclosed at EOF: %s'
                        % ', '.join('<%s>@%d' % (t, ln) for t, ln, _ in c.stack))
        if errs:
            bad += 1
            print('%s:' % path)
            for e in errs[:12]:
                print('   ' + e)
    else:
        c = Checker()
        c.feed(text)
        dupes = {k: v for k, v in c.ids.items() if len(v) > 1}
        if dupes:
            bad += 1
            print('%s: %d duplicated id(s)' % (path, len(dupes)))
            for k, v in sorted(dupes.items()):
                print('   %-24s lines %s' % (k, v))

if bad:
    sys.exit(1)
print('%d file(s) ok' % n)
PY
    out="$TMPDIR_OWNED/html-structure.txt"
    if python3 "$HTML_PROBE" structure >"$out" 2>&1; then
      pass 'html-structure' "$(cat "$out")"
    else
      fail 'html-structure' 'unbalanced tags or a nested .tab-content' "$out"
    fi

    out="$TMPDIR_OWNED/html-ids.txt"
    if python3 "$HTML_PROBE" ids >"$out" 2>&1; then
      pass 'html-ids' "$(cat "$out")"
    else
      fail 'html-ids' 'duplicated id attributes (getElementById takes the first)' "$out"
    fi
  fi

  if ! have python3; then
    skip 'locale-json/symmetry' 'python3 not on PATH'
  else
    out="$TMPDIR_OWNED/locale.txt"
    : >"$out"
    loc_bad=0 loc_n=0
    while IFS= read -r f; do
      loc_n=$((loc_n + 1))
      if ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$f" >>"$out" 2>&1; then
        loc_bad=$((loc_bad + 1)); printf '  ^ in %s\n' "$f" >>"$out"
      fi
    done < <(find web/locales -name '*.json' | sort)
    if [[ $loc_bad -eq 0 ]]; then pass 'locale-json' "$loc_n file(s) valid"
    else fail 'locale-json' "$loc_bad of $loc_n file(s) invalid" "$out"; fi

    # en vs zh must match EXACTLY: both are maintained in full, so a key present
    # in one and missing in the other is a real regression. vi.json is a known
    # partial translation (it arrived with the v1.2.8 merge covering the incoming
    # side's key set only), so it is reported as coverage, never as a failure —
    # otherwise the gate would be red from the moment it was added.
    out="$TMPDIR_OWNED/sym.txt"
    if python3 - "$REPO_ROOT" >"$out" 2>&1 <<'PY'; then
import json, sys, os
root = sys.argv[1]
def leaves(o, pre=""):
    if isinstance(o, dict):
        s = set()
        for k, v in o.items():
            s |= leaves(v, f"{pre}.{k}" if pre else k)
        return s
    return {pre}
def load(lang):
    p = os.path.join(root, "web", "locales", f"{lang}.json")
    with open(p, encoding="utf-8") as fh:
        return leaves(json.load(fh))
en, zh = load("en"), load("zh")
missing, extra = sorted(en - zh), sorted(zh - en)
if missing or extra:
    print(f"en has {len(en)} keys, zh has {len(zh)}")
    for k in missing[:15]: print(f"  missing in zh: {k}")
    for k in extra[:15]:   print(f"  missing in en: {k}")
    sys.exit(1)
print(f"en == zh ({len(en)} leaf keys)")
vi_path = os.path.join(root, "web", "locales", "vi.json")
if os.path.exists(vi_path):
    vi = load("vi")
    print(f"vi coverage: {len(vi & en)}/{len(en)} keys (partial by design, not gated)")
PY
      pass 'locale-symmetry' "$(head -1 "$out")"
      tail -n +2 "$out" | sed 's/^/        /'
    else
      fail 'locale-symmetry' 'en.json and zh.json key sets differ' "$out"
    fi
  fi
fi

# ------------------------------------------------------------ compose ---------
if [[ $RUN_COMPOSE -eq 1 ]]; then
  section 'Deploy manifest'
  if [[ ! -f docker-compose.yml ]]; then
    skip 'yaml/config' 'no docker-compose.yml'
  else
    if have python3; then
      out="$TMPDIR_OWNED/dc-yaml.txt"
      if python3 -c 'import yaml,sys; yaml.safe_load(open("docker-compose.yml"))' >"$out" 2>&1; then
        pass 'yaml' 'docker-compose.yml parses'
      else
        fail 'yaml' 'docker-compose.yml is not valid YAML' "$out"
      fi
    else
      skip 'yaml' 'python3 not on PATH'
    fi

    # `docker compose config` is the authoritative check: it applies Compose's own
    # schema on top of YAML validity, and it does NOT contact the daemon, start
    # anything, or read data/. Safe in the gate.
    if have docker; then
      out="$TMPDIR_OWNED/dc-config.txt"
      if docker compose config >"$out" 2>&1; then pass 'config' 'docker compose config resolves'
      else fail 'config' 'docker compose config rejected the file' "$out"; fi
    else
      skip 'config' 'docker not on PATH'
    fi
  fi
fi

# ---------------------------------------------------------------- git ---------
if [[ $RUN_GIT -eq 1 ]]; then
  section 'Tree hygiene'
  if ! have git || [[ ! -d .git ]]; then
    skip 'whitespace' 'not a git work tree'
  else
    out="$TMPDIR_OWNED/ws.txt"
    if git diff --check >"$out" 2>&1; then pass 'whitespace' 'git diff --check clean'
    else fail 'whitespace' 'trailing whitespace / conflict markers in the diff' "$out"; fi
  fi
fi

# -------------------------------------------------------------- report --------
printf '\n%s─────────────────────────────%s\n' "$C_DIM" "$C_Z"
printf '  %spassed%s %d   %sfailed%s %d   %sskipped%s %d\n' \
  "$C_OK" "$C_Z" "$PASS_N" "$C_NO" "$C_Z" "$FAIL_N" "$C_SK" "$C_Z" "$SKIP_N"
if [[ $FAIL_N -gt 0 ]]; then
  printf '  failed: %s\n' "${FAILED_NAMES[*]}"
  exit 1
fi
printf '  gate green\n'
exit 0
