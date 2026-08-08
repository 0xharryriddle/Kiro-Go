# 02 — The verification gate

Goal: know exactly what must be green before you commit, why the Go tools alone
are not enough, and how to trust the gate itself.

## Run it

```bash
./scripts/verify.sh              # everything except -race (~1 min)
./scripts/verify.sh --race       # add the race detector (minutes)
./scripts/verify.sh --go         # Go checks only
./scripts/verify.sh --web        # JS + locale checks only
./scripts/verify.sh --compose    # docker-compose validity only
./scripts/verify.sh --list       # print the checks and exit
```

Exit code is 0 only when every selected check passed. Failures are accumulated
and reported together — the script deliberately does **not** `set -e`, so one
failure never hides the rest.

## Why `go test` is not the gate

This is the concrete reason, not a principle. The `hian699` v1.2.8 merge produced
a tree where all three Go commands passed:

```
go build ./...   → Success
go vet ./...     → No issues found
go test ./...    → 1328 passed
```

…and yet two artifacts were broken:

- **`web/app.js` did not parse.** Seven separate splices — duplicate `const`
  declarations, missing commas in object literals, a consumed function
  declaration, a consumed closing brace. A `SyntaxError` anywhere in that file
  means the browser loads *none* of it, so every admin panel control was dead.
- **`docker-compose.yml` was invalid YAML.** A sequence item landed inside the
  `healthcheck:` mapping, so `docker compose config` failed and the entire deploy
  path was broken.

`.github/workflows/ci.yml` runs build, vet, gofmt, and `go test -race` — Go only
(`ci.yml:47-85`). So CI would have passed this tree too. Local gates are the only
thing that catches the non-Go half.

## The twelve checks

```
 go      build            go build ./...
 go      vet              go vet ./...
 go      gofmt            gofmt -l . must be empty
 go      test             go test ./... -count=1
 go      race             go test ./... -race -count=1        (only with --race)
 web     js-parse         node --check on every web/**/*.js
 web     html-structure   every web/*.html: balanced tags, no nested .tab-content
 web     html-ids         every web/*.html: no duplicated id attribute
 web     locale-json      every web/locales/*.json parses
 web     locale-symmetry  en.json and zh.json leaf-key sets must match exactly
 compose yaml             docker-compose.yml parses as YAML
 compose config           docker compose config resolves        (needs docker)
 git     whitespace       git diff --check finds no bad whitespace
```

A few of these have subtleties worth stating:

**`gofmt`** exits 0 even when it lists unformatted files, so the *emptiness of
stdout* is the assertion, not the exit code. Getting this wrong produces a check
that can never fail.

**`locale-symmetry`** gates `en` against `zh` only. Both are maintained in full
(1152 leaf keys each), so a key in one and not the other is a real regression.
`vi.json` is reported as coverage — currently **705/1152** — and never fails the
gate: it arrived with the v1.2.8 merge covering the incoming side's key set, and
the 447-key gap is this fork's own features, never translated. Gating on it would
have made the gate red from the moment it was added, which is how gates get
disabled.

**`compose config`** is the authoritative Compose check: it applies Compose's own
schema on top of YAML validity. It does not contact the daemon, start anything,
or read `data/`, so it is safe in a gate.

**`race`** is opt-in because it takes minutes. CI always runs it, so skipping it
locally is fine for a quick loop but not before a push.

## The gate is mutation-proven

A gate that cannot fail is decoration. Every check was verified by introducing a
defect of the class it claims to catch, confirming it goes red, then restoring
and confirming the file is byte-identical. Measured results:

| Check | Mutation | Result |
|---|---|---|
| build | reference an undefined symbol | RED |
| vet | `fmt.Printf("%s", 42)` | RED |
| gofmt | misformatted (but valid) Go | RED |
| test | a `t.Fatal` probe test | RED |
| js-parse | duplicate `const` in `web/toast.js` | RED |
| html-structure | nest a `.tab-content` inside another (the real merge bug) | RED |
| html-structure | drop one `</div>` | RED |
| html-ids | give two real elements the same id | RED |
| locale-json | malformed `vi.json` | RED |
| locale-symmetry | remove one key from `zh.json` | RED |
| compose yaml | sequence item inside `healthcheck:` | RED |
| compose config | `ports:` as a scalar instead of a list | RED |
| whitespace | trailing whitespace in a tracked file | RED |

Two of those mutations are *actual* bugs this repo shipped, reproduced
deliberately — the `compose yaml` one and the nested `.tab-content` one. Those are
the most useful test cases available, because they are known to have occurred
rather than imagined.

`html-ids` also carries a **negative** control, because its first implementation
had a false positive. A regex over the raw file flagged
`web/index-legacy.html:2411/2415`, where two branches of an `if/else` **inside
`<script>`** build the same id and only one ever runs — not a duplicate DOM id at
all. Ids are now collected through `HTMLParser`, which hands script bodies to
`handle_data` as CDATA, so only real elements are seen. The control injects
exactly that shape and asserts the check stays **green**; without it the fix could
silently regress.

Three rules that came out of doing this:

1. **Baseline green first.** If the suite is already red, a "mutation failed"
   result proves nothing.
2. **Restore under hash comparison.** Trusting that a write-back restored the
   original is exactly the assumption that loses work. Compare SHA-256 and abort
   if it differs.
3. **A check that can only go red is also broken.** Pair every positive mutation
   with a negative control — a shape that *looks* like the defect but is legal, and
   must stay green. `html-ids` needed exactly this: it started out flagging ids
   built inside `<script>` branches, which are not duplicate DOM ids.

If you add a check, mutation-prove it in the same pass. An unproven check is a
liability: it grows trust without earning it.

## Reproducing a mutation yourself

The pattern, for `js-parse`:

```bash
cp web/toast.js /tmp/toast.bak                       # back up FIRST
printf '\nconst zzDup=1;\nconst zzDup=2;\n' >> web/toast.js
./scripts/verify.sh --web ; echo "exit=$?"           # expect FAIL / exit=1
cp /tmp/toast.bak web/toast.js                       # restore
git diff --quiet web/toast.js && echo "restored clean"
```

Verify the backup exists *before* mutating. A restore that reads a different
filename than the backup wrote is a real failure mode that has cost time on this
repo.

## When a check fails

The failing check prints its own captured output, indented, so the reason is on
screen rather than in a file you have to find. Common cases:

| Symptom | Cause |
|---|---|
| `gofmt` lists files | run `gofmt -w <files>` |
| `js-parse` fails | `node --check web/app.js` for the first error; it reports only one at a time |
| `locale-symmetry` fails | the diff lists the specific missing keys per side |
| `compose yaml` fails | the parser names the line; look for indentation that crossed a mapping/sequence boundary |
| `whitespace` fails | `git diff --check` names line and column |

**`node --check` reports only the first syntax error in a file.** After fixing
one, re-run — there may be more. Worse, the reported line can be far from the
cause: an unclosed brace surfaces as `Unexpected token ')'` at the *last* line of
the file. When that happens, bisect instead of guessing: split the file at
top-level declarations and `node --check` each chunk. That localised an unclosed
function to within a few lines when the reported error was 780 lines away.

## What the gate does not cover

Being explicit so nobody reads a green gate as more than it is:

- **No browser execution, and no HTML parsing at all.** `node --check` proves
  `app.js` *parses*; nothing here parses `index.html`. That blind spot hid a real
  defect: the v1.2.8 merge left `tabApilog` and `tabConsole` **nested inside** the
  hidden `tabLogs` div, so those two admin tabs could never render — a hidden
  parent hides its children whatever their own class says. Both merge parents were
  structurally clean; only the merged result was broken, and every Go check plus
  `node --check` stayed green. It also left 9 duplicated element ids, so
  `getElementById` resolved to whichever came first and two different handlers
  bound the same node.

  Both are fixed. Re-measure structure and ids with:

  ```bash
  grep -oE 'id="[^"]+"' web/index.html | sort | uniq -d    # expect no output
  python3 - <<'EOF'
  from html.parser import HTMLParser
  # any balanced-tag checker; the point is that the gate does not do this for you
  EOF
  ```

  A gate check for HTML structure and id uniqueness is the obvious next addition
  and does not exist yet.
- **No live upstream calls.** Tests use hermetic fixtures. Kiro/Bedrock contract
  drift is invisible here.
- **No `docker compose up`.** `config` validates the manifest; it does not prove
  the container starts. `scripts/deploy.sh` covers that.
- **No `data/` validation.** Deliberate: the gate is read-only and never touches
  production state.

## Before you commit

```bash
./scripts/verify.sh --race    # full gate
git status --porcelain        # no stray probe/temp files staged
```

Check the second one. Reviewer probes and `zz_*` files have been swept into a
commit by `git add -A` on this repo before.

## Next

- [03 — Deploy and rollback](03-deploy-and-rollback.md)
