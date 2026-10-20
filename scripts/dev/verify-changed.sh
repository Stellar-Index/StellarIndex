#!/usr/bin/env bash
# verify-changed.sh [BASE] — the narrowest verification of what you changed, in one screen of output.
#
#   Go:  vet + test -race on the packages the change can reach, selected the way CI selects them
#        (scripts/ci/affected-go-pkgs -worktree: committed, staged, unstaged and untracked edits alike).
#        The full go test output goes to a log file; the screen gets the counts and, on failure, only the
#        failing lines.
#   web: typecheck for each web app touched, plus vitest for web/explorer, summarised the same way.
#
# BASE defaults to origin/main. Runs against the repository that contains the current directory, so the
# fixture test can drive it. Exit 0 only when every step ran and passed: a step that cannot run (no
# node_modules) fails, because "not verified" is not "verified".
# Run: make verify-changed     (VERIFY_CHANGED_ARGS='<ref>' compares against another base)
set -uo pipefail
self_root="$(cd "$(dirname "$0")/../.." && pwd)"
top="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "verify-changed: not inside a git work tree" >&2; exit 2; }
cd "$top" || exit 2
base=${1:-origin/main}
git rev-parse --verify -q "$base^{commit}" >/dev/null || { echo "verify-changed: no such base: $base" >&2; exit 2; }
log_dir=${TMPDIR:-/tmp}/verify-changed
mkdir -p "$log_dir"
stamp=$(date +%Y%m%d-%H%M%S)
status=0

tool="$log_dir/affected-go-pkgs"
(cd "$self_root" && go build -o "$tool" ./scripts/ci/affected-go-pkgs) || exit 2
files=$("$tool" -worktree -files "$base") || exit 2
pkgs=$("$tool" -worktree "$base") || exit 2

if [ -n "$pkgs" ]; then
    # shellcheck disable=SC2086
    if go vet $pkgs 2>"$log_dir/go-vet-$stamp.log"; then
        echo "go vet: ok ($(wc -w <<<"$pkgs" | tr -d ' ') package(s))"
    else
        echo "go vet: FAIL"
        head -60 "$log_dir/go-vet-$stamp.log"
        status=1
    fi
    log=$log_dir/go-test-$stamp.log
    # shellcheck disable=SC2086
    go test -race -timeout 8m $pkgs >"$log" 2>&1
    rc=$?
    echo "go test: $(grep -c '^ok ' "$log") ok, $(grep -c $'^FAIL\t' "$log") FAIL," \
        "$(grep -c 'no test files' "$log") no test files, full output $log"
    if [ "$rc" -ne 0 ]; then
        grep -v -E '^(ok |\? |PASS$)' "$log" | sed -n '1,150p'
        status=1
    fi
else
    echo "go: no package affected"
fi

for app in explorer status; do
    grep -q "^web/$app/" <<<"$files" || continue
    if [ ! -x "web/$app/node_modules/.bin/tsc" ]; then
        echo "web/$app: node_modules missing, run make bootstrap-worktree"
        status=1
        continue
    fi
    log=$log_dir/$app-typecheck-$stamp.log
    if pnpm --dir "web/$app" typecheck >"$log" 2>&1; then
        echo "web/$app typecheck: ok"
    else
        echo "web/$app typecheck: FAIL, full output $log"
        grep -E 'error TS|^\S+\.tsx?\(' "$log" | sed -n '1,60p'
        status=1
    fi
    [ "$app" = explorer ] || continue
    log=$log_dir/vitest-$stamp.log
    if pnpm --dir web/explorer test >"$log" 2>&1; then
        echo "vitest: $(grep -E '^\s*(Test Files|Tests)\s' "$log" | tr -s ' ' | paste -sd ';' -), full output $log"
    else
        echo "vitest: FAIL, full output $log"
        grep -E -A6 '^\s*(❯|×|✗|FAIL\b|⎯+ Failed|AssertionError|Error:|TypeError|Test Files\s|Tests\s)' "$log" | sed -n '1,150p'
        status=1
    fi
done
exit "$status"
