#!/usr/bin/env bash
# lint-changed-test.sh — fixture tests for the changed-file lint dispatcher
# (scripts/dev/lint-changed.sh) and the pre-commit hook installer
# (scripts/dev/install-hooks.sh).
#
# The dispatcher's whole value is selection: the right lints for each changed
# file's type, none for the types no lint covers, cheapest first, and a real
# failure when a real defect is fed to it. Each of those is pinned here
# against fixture repositories, so a dispatch-table edit that quietly drops a
# type (or starts running go vet on a shell edit) fails this test rather than
# being noticed at minute ten of a push:
#
#   - selection by type, in one mixed diff and in isolation per type;
#   - a .md-only diff runs lint-doc-links scoped to the changed files, and
#     defers only lint-docs (which takes no file list), never both;
#   - a *-test.sh is run, and runs last;
#   - discovery: staged, a commit range, the default fallback, an explicit
#     list, and the refusals for each;
#   - RED: a pipefail script piping into head fails the run and names
#     lint-shell-sigpipe; the same script written correctly passes;
#   - a script without pipefail does not trip the sigpipe gate's vacuity
#     guard (the gate is skipped with a reason, bash -n still runs);
#   - the hook installer: install, refresh, refuse a foreign hook, refuse a
#     dangling core.hooksPath, honour a real one, uninstall only its own; and
#     the installed hook calls the dispatcher with --staged, blocks the commit
#     on its exit code, is bypassed by --no-verify, and is a no-op in a
#     repository that has no dispatcher.
#
# Fixture repositories are created under mktemp with global and system git
# config masked, so a contributor's hooks, signing or template settings
# cannot reach them. The offending fixture bodies below are written on ONE
# source line each so the `# sigpipe-ok:` marker can sit outside the quoted
# body: scripts/dev is a lint-shell-sigpipe root, and the fixture must
# contain the shape the gate hunts for.
#
# Run: bash scripts/dev/lint-changed-test.sh
set -uo pipefail

# `git init` honours an INHERITED GIT_DIR ahead of its own `-C`, and a git
# hook exports GIT_DIR/GIT_INDEX_FILE. lint-changed dispatches test scripts,
# and the pre-commit hook runs lint-changed — so without this a fixture's
# init re-initialises the REAL repository: core.bare set on the live
# checkout, fixture commits on main, and git says only "warning: re-init".
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
DISPATCH="$PWD/scripts/dev/lint-changed.sh"
INSTALL="$PWD/scripts/dev/install-hooks.sh"

# Physical path: macOS mktemp answers under /var, a symlink to /private/var,
# and git reports the resolved form, which the path assertions compare against.
TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid

pass=0
fail=0
ok()  { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { local d="$1"; shift; echo "  FAIL $d"; local l; for l in "$@"; do echo "       $l"; done; fail=$((fail + 1)); }

expect_exit() { # expect_exit <desc> <want> <got>
    if [ "$3" -eq "$2" ]; then ok "$1"; else bad "$1" "exit $3, want $2"; fi
}
expect_has() { # expect_has <desc> <needle> <haystack>
    if grep -qF -- "$2" <<<"$3"; then ok "$1"; else bad "$1" "missing: $2"; fi
}
expect_not() { # expect_not <desc> <needle> <haystack>
    if grep -qF -- "$2" <<<"$3"; then bad "$1" "present but must not be: $2"; else ok "$1"; fi
}
line_of() { # line_of <needle> <haystack> — 1-based line number of the first match, or 0
    local n
    n="$(grep -nF -- "$1" <<<"$2" | sed -n '1p' | cut -d: -f1)"
    echo "${n:-0}"
}

new_repo() { # new_repo <dir> — an initialised repository with one commit
    mkdir -p "$1" && git -C "$1" init -q && git -C "$1" commit -q --allow-empty -m init
}
put() { # put <repo> <relpath> <body>
    mkdir -p "$(dirname "$1/$2")"
    printf '%s\n' "$3" > "$1/$2"
}

# ── Selection ──────────────────────────────────────────────────────────────
echo "lint-changed-test: selection by file type"

R="$TMP/mixed"; new_repo "$R"
put "$R" tools/run.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho run'
put "$R" pkg/thing.go $'package pkg\n'
put "$R" pkg/thing_test.go $'package pkg\n'
put "$R" .github/workflows/w.yml $'name: w\non: push\njobs: {}'
put "$R" migrations/0001_x.up.sql 'CREATE TABLE x (id int);'
put "$R" docs/note.md '# note'
put "$R" scripts/ci/x.baseline 'entry'
put "$R" scripts/dev/verify.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho verify'
put "$R" configs/prometheus/rules.r1/meta.yml $'groups:\n  - name: meta\n    rules: []\n'
put "$R" configs/ansible/roles/x/tasks/main.yml $'- name: noop\n  ansible.builtin.debug:\n    msg: hi\n'
put "$R" data.json '{}'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"; rc=$?
expect_exit "a mixed staged diff plans without error" 0 "$rc"
expect_has "discovery reports the staged count" "lint-changed: 11 changed file(s), staged" "$out"
expect_has ".sh -> bash -n on the file" "plan  bash -n: bash -n tools/run.sh" "$out"
# git lists staged paths in index order (alphabetical), and the plan keeps it.
expect_has ".sh -> lint-shell-sigpipe scoped to the pipefail scripts" "lint-shell-sigpipe.sh scripts/dev/verify.sh tools/run.sh" "$out"
if command -v shellcheck >/dev/null 2>&1; then
    expect_has ".sh -> shellcheck -x on the files" "plan  shellcheck: shellcheck -x scripts/dev/verify.sh tools/run.sh" "$out"
else
    expect_has ".sh -> shellcheck deferred when absent" "skip  shellcheck:" "$out"
fi
expect_has ".go -> gofumpt on the files" "gofumpt -l pkg/thing.go pkg/thing_test.go" "$out"
expect_has ".go -> goimports with the module's -local" "goimports -l -local github.com/Stellar-Index/StellarIndex pkg/thing.go" "$out"
expect_has ".go -> go vet on the package, once" "plan  go vet: go vet ./pkg   (1 package(s))" "$out"
expect_has ".go -> go build on the package" "plan  go build: go build ./pkg" "$out"
expect_has ".go -> lint-http-timeouts scoped to the package dir" "lint-http-timeouts.sh ./pkg" "$out"
expect_has ".go -> lint-lexicon (whole tree)" "plan  lint-lexicon:" "$out"
expect_has "workflow -> lint-actions-pinning scoped" "lint-actions-pinning.sh .github/workflows/w.yml" "$out"
# The sigpipe gate gets the workflows DIRECTORY appended after the changed
# pipefail scripts, not the changed workflow file. The trailing "   (" is
# the start of the step note, so it proves the argument ENDED at the
# directory. Before this row nothing sent .yml to the gate at all: a commit
# touching six workflow files ran it as "0 pipefail run: block(s) in 0
# workflow file(s)", blind to the class that failed the v0.69.0 deploy.
expect_has "workflow -> lint-shell-sigpipe over the workflows DIRECTORY, after the scripts" "lint-shell-sigpipe.sh scripts/dev/verify.sh tools/run.sh .github/workflows   (" "$out"
if command -v actionlint >/dev/null 2>&1; then
    expect_has "workflow -> actionlint with embedded shellcheck off" "actionlint -shellcheck= .github/workflows/w.yml" "$out"
fi
if command -v zizmor >/dev/null 2>&1; then
    expect_has "workflow -> zizmor offline at CI's confidence floor" "zizmor --offline --min-confidence medium .github/workflows/w.yml" "$out"
fi
expect_has "migration -> lint-migrations" "plan  lint-migrations:" "$out"
expect_has "migration -> lint-migration-immutability" "plan  lint-migration-immutability:" "$out"
expect_has "migration -> lint-migration-commands" "plan  lint-migration-commands:" "$out"
expect_has "migration -> lint-migration-compat --staged in staged mode" "lint-migration-compat.sh --staged" "$out"
# Alert rules. Before 2026-09-08 a rules-YAML diff selected NO monitoring
# gate at all — a 16-file change touching 14 rule YAMLs planned exactly one
# lint, for its two .md files (ENG-14). Each of these four pins one gate to
# the type; the two deferrals are asserted too, because "deferred with a
# reason" and "never considered" look identical in a summary line.
expect_has "rules yaml -> lint-rule-equivalence" "plan  lint-rule-equivalence:" "$out"
expect_has "rules yaml -> lint-alerts-catalog" "plan  lint-alerts-catalog:" "$out"
expect_has "rules yaml -> lint-runbook-annotations" "plan  lint-runbook-annotations:" "$out"
expect_has "rules yaml -> lint-rule-structure" "plan  lint-rule-structure:" "$out"
expect_has "rules yaml -> lint-metric-refs deferred on cost, with the reason" "skip  lint-metric-refs:" "$out"
expect_has "rules yaml -> promtool deferred, and the gap is named" "skip  monitoring-check:" "$out"
# Ansible. Same gap as the rules block above: a change under
# configs/ansible/ selected no ansible gate at all, while both gates are
# under half a second whole-tree.
expect_has "ansible -> lint-ansible-tasks" "plan  lint-ansible-tasks:" "$out"
expect_has "ansible -> lint-jinja-templates" "plan  lint-jinja-templates:" "$out"
expect_has "verify.sh -> check-verify-parity" "plan  check-verify-parity:" "$out"
expect_has "baseline -> lint-baseline-growth deferred on staged edits, with the reason" "skip  lint-baseline-growth: reads the Baseline-Growth commit trailer" "$out"
expect_has ".md -> lint-doc-links scoped to the changed file" "lint-doc-links.sh docs/note.md" "$out"
expect_has ".md -> lint-docs still deferred (no file list)" "skip  lint-docs: 1 .md file(s) changed; lint-docs" "$out"
expect_has "an uncovered type is named, not silently dropped" "skip  data.json: no changed-file lint applies to this type" "$out"
expect_not "no test script changed -> none is run" "plan  test-script:" "$out"
expect_not "no lake-table reference -> lint-lake-dedup is not selected" "lint-lake-dedup" "$out"
expect_has "the plan line accounts for lints, files and deferrals" "lint-changed: plan — " "$out"

# Cheapest first: syntax before shellcheck, shell before Go, vet before build.
n_bash="$(line_of 'plan  bash -n:' "$out")"
n_sig="$(line_of 'plan  lint-shell-sigpipe:' "$out")"
n_fmt="$(line_of 'plan  gofumpt:' "$out")"
n_vet="$(line_of 'plan  go vet:' "$out")"
n_build="$(line_of 'plan  go build:' "$out")"
if [ "$n_bash" -gt 0 ] && [ "$n_bash" -lt "$n_sig" ] && [ "$n_sig" -lt "$n_fmt" ] && [ "$n_fmt" -lt "$n_vet" ] && [ "$n_vet" -lt "$n_build" ]; then
    ok "order is bash -n < sigpipe < gofumpt < go vet < go build"
else
    bad "order is bash -n < sigpipe < gofumpt < go vet < go build" "lines: bash-n=$n_bash sigpipe=$n_sig gofumpt=$n_fmt vet=$n_vet build=$n_build"
fi

echo "lint-changed-test: selection in isolation (unrelated lints are skipped)"

R="$TMP/goonly"; new_repo "$R"
put "$R" internal/x/x.go $'package x\n'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"
expect_has "go-only: gofumpt selected" "plan  gofumpt:" "$out"
expect_has "go-only: go vet on ./internal/x" "go vet ./internal/x" "$out"
expect_not "go-only: no bash -n" "plan  bash -n:" "$out"
expect_not "go-only: no shellcheck" "plan  shellcheck:" "$out"
expect_not "go-only: no lint-shell-sigpipe" "lint-shell-sigpipe" "$out"
expect_not "go-only: no actionlint" "actionlint" "$out"
expect_not "go-only: no migration gates" "lint-migrations" "$out"
expect_not "go-only: no check-verify-parity" "check-verify-parity" "$out"

R="$TMP/shonly"; new_repo "$R"
put "$R" bin/x.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho x'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"
expect_has "sh-only: bash -n selected" "plan  bash -n: bash -n bin/x.sh" "$out"
expect_not "sh-only: no gofumpt" "gofumpt" "$out"
expect_not "sh-only: no go vet" "go vet" "$out"
expect_not "sh-only: no lint-lexicon" "lint-lexicon" "$out"
expect_not "sh-only: no actionlint" "actionlint" "$out"

R="$TMP/wfonly"; new_repo "$R"
put "$R" .github/workflows/a.yml $'name: a\non: push\npermissions:\n  contents: read\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1  # v7.0.1\n      - run: |\n          set -euo pipefail\n          echo ok'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"
expect_has "workflow-only: lint-shell-sigpipe is still selected with no .sh in the diff" "lint-shell-sigpipe.sh .github/workflows   (" "$out"
expect_not "workflow-only: no gofumpt" "gofumpt" "$out"
expect_not "workflow-only: no migration gates" "lint-migrations" "$out"

R="$TMP/mdonly"; new_repo "$R"
put "$R" docs/a.md '# a'
put "$R" CHANGELOG.md '# changelog'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"; rc=$?
expect_exit "md-only: plans cleanly" 0 "$rc"
expect_has "md-only: lint-doc-links scoped to both files, one deferral — never reads as fully linted" "lint-changed: plan — 1 lint(s) over 2 changed file(s), 1 deferred" "$out"
expect_has "md-only: lint-doc-links scoped to both changed files" "lint-doc-links.sh CHANGELOG.md docs/a.md" "$out"
expect_has "md-only: lint-docs still names the file count and itself" "skip  lint-docs: 2 .md file(s) changed; lint-docs" "$out"

R="$TMP/lake"; new_repo "$R"
put "$R" internal/r/reader.go $'package r\n\nconst q = "SELECT count() FROM stellar.transactions"\n'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"
expect_has "a .go naming a duplicate-bearing table -> lint-lake-dedup" "plan  lint-lake-dedup:" "$out"

R="$TMP/testsh"; new_repo "$R"
put "$R" tools/a.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho a'
put "$R" tools/a-test.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho test'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"
expect_has "a *-test.sh is run" "plan  test-script: bash tools/a-test.sh" "$out"
plans="$(grep '^plan  ' <<<"$out")"
n_test="$(line_of 'plan  test-script:' "$plans")"
n_last="$(grep -c . <<<"$plans")"
if [ "$n_test" -gt 0 ] && [ "$n_test" -eq "$n_last" ]; then
    ok "the test script runs last"
else
    bad "the test script runs last" "test-script at plan line $n_test of $n_last"
fi

# ── Discovery ──────────────────────────────────────────────────────────────
echo "lint-changed-test: discovery"

R="$TMP/range"; new_repo "$R"
first="$(git -C "$R" rev-parse HEAD)"
put "$R" scripts/ci/y.baseline 'entry'
put "$R" tools/r.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho r'
git -C "$R" add -A && git -C "$R" commit -q -m 'second'
out="$(cd "$R" && "$DISPATCH" --staged --plan 2>&1)"; rc=$?
expect_exit "--staged with a clean index exits 0" 0 "$rc"
expect_has "--staged with a clean index reports nothing to lint" "lint-changed: OK — 0 lint(s) over 0 changed file(s): nothing to lint" "$out"
out="$(cd "$R" && "$DISPATCH" --base "$first" --plan 2>&1)"; rc=$?
expect_exit "--base <rev> plans" 0 "$rc"
expect_has "--base reports the range and the merge-base" "lint-changed: 2 changed file(s), ${first}...HEAD (merge-base ${first:0:12})" "$out"
expect_has "--base runs lint-baseline-growth with BASE_SHA set to the merge-base" "plan  lint-baseline-growth: env BASE_SHA=${first} " "$out"
out="$(cd "$R" && "$DISPATCH" --plan 2>&1)"; rc=$?
expect_exit "no mode, nothing staged, no origin/main -> exit 2" 2 "$rc"
expect_has "the refusal names origin/main" "revision 'origin/main' does not resolve" "$out"
put "$R" tools/s.sh $'#!/usr/bin/env bash\nset -euo pipefail\necho s'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --plan 2>&1)"; rc=$?
expect_exit "no mode with something staged -> staged mode" 0 "$rc"
expect_has "staged mode chosen by default when the index is dirty" "lint-changed: 1 changed file(s), staged" "$out"
out="$(cd "$R" && "$DISPATCH" --plan -- tools/nosuch.sh 2>&1)"; rc=$?
expect_exit "an explicit file that does not exist -> exit 2" 2 "$rc"
out="$(cd "$R" && "$DISPATCH" --bogus 2>&1)"; rc=$?
expect_exit "an unknown flag -> exit 2" 2 "$rc"

# ── Red: a real defect, a real run ─────────────────────────────────────────
echo "lint-changed-test: a pipefail script piping into head"

R="$TMP/red"; new_repo "$R"
put "$R" tools/offender.sh $'#!/usr/bin/env bash\nset -euo pipefail\nprintf \'%s\\n\' a b c | head -n 1'   # sigpipe-ok: fixture text, scanned by the gate and never executed
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged 2>&1)"; rc=$?
expect_exit "the run FAILS" 1 "$rc"
expect_has "the failure names lint-shell-sigpipe" "lint-changed: FAIL lint-shell-sigpipe" "$out"
expect_has "the gate's own diagnosis is shown" "tools/offender.sh:3 pipes into an EARLY-EXIT consumer" "$out"
expect_has "bash -n still passed (the failure is the sigpipe class, not syntax)" "lint-changed: ok   bash -n" "$out"
expect_has "the summary counts one failure and names it" "of 4 lint(s) failed over 1 changed file(s)" "$out"

R="$TMP/green"; new_repo "$R"
put "$R" tools/fixed.sh $'#!/usr/bin/env bash\nset -euo pipefail\nprintf \'%s\\n\' a b c > "$1"\nhead -n 1 "$1"'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged 2>&1)"; rc=$?
expect_exit "the write-then-slice rewrite passes" 0 "$rc"
expect_has "the summary accounts for every lint that ran" "lint-changed: OK — 4 lint(s) over 1 changed file(s)" "$out"

R="$TMP/loose"; new_repo "$R"
put "$R" tools/loose.sh $'#!/usr/bin/env bash\necho hi'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged 2>&1)"; rc=$?
expect_exit "a script without pipefail passes (the sigpipe gate's vacuity guard is not tripped)" 0 "$rc"
expect_has "the sigpipe gate is skipped with the reason" "skip  lint-shell-sigpipe: none of the changed scripts sets pipefail or -e" "$out"
expect_has "bash -n ran on it" "lint-changed: ok   bash -n" "$out"

echo "lint-changed-test: a pipefail run: block piping into head"

# The workflow half of the same gate, and the reason the dispatch row exists.
# `sort | head -n 10` in a step that merely LISTED the staged migrations
# killed the v0.69.0 deploy; the gate has covered `run:` blocks since
# 2026-09-10, but no dispatch row sent .yml to it, so the pre-commit path
# never saw one. Written on ONE source line so the `# sigpipe-ok:` marker can
# sit outside the quoted body, as for the .sh fixtures above.
#
# The path:line assertion is the discriminating one. lint-actions-pinning
# reds here too, and only as an artefact of the fixture: unlike
# lint-shell-sigpipe it `cd`s to the checkout IT lives in before resolving
# its roots, so the fixture's workflow path does not exist for it. That is
# why the workflow selection above is only PLANNED, and why the clean-run
# half of this property is exercised against the real tree just below.
R="$TMP/wfred"; new_repo "$R"
put "$R" .github/workflows/bad.yml $'name: bad\non: push\npermissions:\n  contents: read\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1  # v7.0.1\n      - run: |\n          set -euo pipefail\n          printf \'%s\\n\' a b c | head -n 1'   # sigpipe-ok: fixture text, scanned by the gate and never executed
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged 2>&1)"; rc=$?
expect_exit "a workflow-only diff with a pipefail run: block piping into head FAILS" 1 "$rc"
expect_has "the failure names lint-shell-sigpipe" "lint-changed: FAIL lint-shell-sigpipe" "$out"
expect_has "the report resolves in the WORKFLOW file, at the offending source line" ".github/workflows/bad.yml:12 pipes into an EARLY-EXIT consumer" "$out"

# The other half: the workflows DIRECTORY is the root and not the changed
# file. Handed a lone workflow with no pipefail `run:` block the gate exits 1
# — "the workflow half of the gate would be vacuous" — which is the right
# answer for a whole-tree run and a false red on an innocent pre-commit edit.
# Against THIS repository, because a fixture cannot get a clean workflow run
# out of the dispatcher (see lint-actions-pinning above), and the subject is
# DERIVED rather than named: if every workflow ever sets pipefail there is no
# live case, and that says so out loud instead of passing on nothing.
quiet_wf=""
for w in .github/workflows/*.yml; do
    if ! bash scripts/ci/lint-shell-sigpipe.sh "$w" >/dev/null 2>&1; then quiet_wf="$w"; break; fi
done
if [ -n "$quiet_wf" ]; then
    out="$("$DISPATCH" -- "$quiet_wf" 2>&1)"; rc=$?
    expect_exit "editing ${quiet_wf} (no pipefail run: block, so a vacuity FAIL scoped to itself) does not red the run" 0 "$rc"
    expect_has "because the gate got the DIRECTORY and saw the other workflows' pipefail blocks" "lint-changed: ok   lint-shell-sigpipe" "$out"
else
    echo "  note every workflow in this tree sets pipefail, so the vacuity case has nothing live to pin today"
fi

R="$TMP/syntax"; new_repo "$R"
put "$R" tools/broken.sh $'#!/usr/bin/env bash\nset -euo pipefail\nif true; then echo x'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged 2>&1)"; rc=$?
expect_exit "a syntax error fails the run" 1 "$rc"
expect_has "bash -n is the step that failed" "lint-changed: FAIL bash -n" "$out"

R="$TMP/other"; new_repo "$R"
put "$R" data.json '{}'
git -C "$R" add -A
out="$(cd "$R" && "$DISPATCH" --staged 2>&1)"; rc=$?
expect_exit "an uncovered type alone exits 0" 0 "$rc"
expect_has "and says that no lint applied rather than claiming a pass" "lint-changed: OK — 0 lint(s) over 1 changed file(s): no changed-file lint applies" "$out"

# ── The hook installer ─────────────────────────────────────────────────────
echo "lint-changed-test: install-hooks"

R="$TMP/hk"; new_repo "$R"
out="$(cd "$R" && "$INSTALL" 2>&1)"; rc=$?
expect_exit "install into a fresh repository" 0 "$rc"
hook="$R/.git/hooks/pre-commit"
if [ -x "$hook" ] && grep -qF 'stellarindex-lint-changed-hook' "$hook"; then
    ok "the hook exists, is executable and carries the marker"
else
    bad "the hook exists, is executable and carries the marker"
fi
expect_has "the report says installed and where" "install-hooks: installed $hook" "$out"
out="$(cd "$R" && "$INSTALL" 2>&1)"; rc=$?
expect_exit "a second install is idempotent" 0 "$rc"
expect_has "and reports a refresh" "install-hooks: refreshed" "$out"

git -C "$R" worktree add -q "$TMP/hk-wt" HEAD
out="$(cd "$TMP/hk-wt" && "$INSTALL" 2>&1)"; rc=$?
expect_exit "install from a linked worktree succeeds" 0 "$rc"
expect_has "and lands in the shared hooks directory, not the worktree" "$hook" "$out"

# The hook's contract, with a stub dispatcher that records its argv. The
# single quotes are the point: the stub's "$@" must expand when the hook
# runs it, not here.
mkdir -p "$R/scripts/dev"
# shellcheck disable=SC2016
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$@" > "$(git rev-parse --show-toplevel)/called"' 'exit 3' > "$R/scripts/dev/lint-changed.sh"
chmod +x "$R/scripts/dev/lint-changed.sh"
put "$R" x.txt 'x'
git -C "$R" add x.txt
before="$(git -C "$R" rev-parse HEAD)"
out="$(cd "$R" && git commit -q -m 'blocked' 2>&1)"; rc=$?
expect_exit "a commit is blocked by the dispatcher's exit code" 1 "$rc"
if [ "$(git -C "$R" rev-parse HEAD)" = "$before" ]; then ok "no commit was created"; else bad "no commit was created"; fi
if [ -f "$R/called" ] && [ "$(cat "$R/called")" = "--staged" ]; then
    ok "the hook ran the dispatcher with exactly --staged"
else
    bad "the hook ran the dispatcher with exactly --staged" "called: $(cat "$R/called" 2>/dev/null)"
fi
rm -f "$R/called"
out="$(cd "$R" && git commit -q --no-verify -m 'bypassed' 2>&1)"; rc=$?
expect_exit "--no-verify bypasses the hook" 0 "$rc"
if [ ! -e "$R/called" ]; then ok "and the dispatcher was not run"; else bad "and the dispatcher was not run"; fi
rm -f "$R/scripts/dev/lint-changed.sh"
put "$R" y.txt 'y'
git -C "$R" add y.txt
out="$(cd "$R" && git commit -q -m 'no dispatcher' 2>&1)"; rc=$?
expect_exit "a repository without the dispatcher commits normally (hook is a no-op)" 0 "$rc"

out="$(cd "$R" && "$INSTALL" --uninstall 2>&1)"; rc=$?
expect_exit "uninstall removes the hook" 0 "$rc"
if [ ! -e "$hook" ]; then ok "the hook file is gone"; else bad "the hook file is gone"; fi
out="$(cd "$R" && "$INSTALL" --uninstall 2>&1)"; rc=$?
expect_exit "uninstall with no hook is not an error" 0 "$rc"
expect_has "and says so" "nothing to remove" "$out"

R="$TMP/hk-foreign"; new_repo "$R"
mkdir -p "$R/.git/hooks"
printf '%s\n' '#!/bin/sh' 'exit 0' > "$R/.git/hooks/pre-commit"
chmod +x "$R/.git/hooks/pre-commit"
out="$(cd "$R" && "$INSTALL" 2>&1)"; rc=$?
expect_exit "a foreign pre-commit hook is refused" 1 "$rc"
expect_has "the refusal prints the line to chain" "lint-changed.sh\" --staged || exit 1" "$out"
if [ "$(cat "$R/.git/hooks/pre-commit")" = $'#!/bin/sh\nexit 0' ]; then ok "the foreign hook is untouched"; else bad "the foreign hook is untouched"; fi
out="$(cd "$R" && "$INSTALL" --uninstall 2>&1)"; rc=$?
expect_exit "uninstall refuses to remove a foreign hook" 1 "$rc"
if [ -e "$R/.git/hooks/pre-commit" ]; then ok "and leaves it in place"; else bad "and leaves it in place"; fi

R="$TMP/hk-dangling"; new_repo "$R"
git -C "$R" config core.hooksPath "$TMP/does-not-exist/hooks"
out="$(cd "$R" && "$INSTALL" 2>&1)"; rc=$?
expect_exit "core.hooksPath at a missing directory is refused" 1 "$rc"
expect_has "the refusal names the setting" "core.hooksPath is set to '$TMP/does-not-exist/hooks'" "$out"
if [ ! -e "$TMP/does-not-exist" ]; then ok "and nothing was created there"; else bad "and nothing was created there"; fi

R="$TMP/hk-custom"; new_repo "$R"
mkdir -p "$TMP/customhooks"
git -C "$R" config core.hooksPath "$TMP/customhooks"
out="$(cd "$R" && "$INSTALL" 2>&1)"; rc=$?
expect_exit "an existing core.hooksPath is honoured" 0 "$rc"
if [ -x "$TMP/customhooks/pre-commit" ]; then ok "the hook was written there"; else bad "the hook was written there"; fi

# ── The real tree ──────────────────────────────────────────────────────────
# Not this file: a *-test.sh is RUN by the dispatcher, and this one running
# itself would recurse.
echo "lint-changed-test: the dispatcher and the installer pass their own lints"
out="$("$DISPATCH" -- scripts/dev/lint-changed.sh scripts/dev/install-hooks.sh 2>&1)"; rc=$?
expect_exit "lint-changed over its own two scripts passes" 0 "$rc"
expect_has "and accounts for what it ran (bash -n twice, sigpipe, fixture-isolation, shellcheck)" "lint-changed: OK — 5 lint(s) over 2 changed file(s)" "$out"

echo "lint-changed-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
