#!/usr/bin/env bash
# doc-pinned-tests.sh — run the Go tests that read a changed markdown file.
#
# A docs-only PR sets preflight go=false and skips the `test` job, yet Go
# tests read markdown at test time (internal/ops/chops pins strings in the
# runbooks). #2440 dropped two of those strings and went green. Putting
# **/*.md in the `go` filter would make every docs PR pay for the full
# matrix, so this selects only the packages whose *_test.go mention a
# changed .md by repo path or basename.
#
# Usage:
#   scripts/ci/doc-pinned-tests.sh <base-rev>        select, then go test
#   scripts/ci/doc-pinned-tests.sh --list <base-rev> print the packages only
#   scripts/ci/doc-pinned-tests.sh --files <f.md>... select from a file list
#
# Basename matching over-selects on a generic name (README.md); the cost is
# an extra package's tests, never a missed one.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

list_only=0
case "${1:-}" in
    --list) list_only=1; shift ;;
esac

md=()
if [ "${1:-}" = "--files" ]; then
    shift
    for f in "$@"; do case "$f" in *.md) md+=("$f") ;; esac; done
else
    base="${1:?usage: doc-pinned-tests.sh [--list] <base-rev> | --files <f.md>...}"
    files="$(git diff --name-only --no-renames "$base" HEAD -- '*.md')"
    while IFS= read -r f; do [ -n "$f" ] && md+=("$f"); done <<<"$files"
fi

dirs=""
for f in ${md[@]+"${md[@]}"}; do
    rc=0
    hits="$(git grep -l -F -e "$f" -e "$(basename "$f")" -- '*_test.go')" || rc=$?
    [ "$rc" -le 1 ] || { echo "doc-pinned-tests: git grep failed (exit $rc)" >&2; exit "$rc"; }
    while IFS= read -r t; do
        [ -n "$t" ] && dirs="${dirs}$(dirname "$t")"$'\n'
    done <<<"$hits"
done

pkgs=()
while IFS= read -r d; do
    [ -n "$d" ] || continue
    rc=0
    err="$(go list "./$d" 2>&1 >/dev/null)" || rc=$?
    if [ "$rc" -ne 0 ]; then
        # A dir whose only files are build-tagged out is not a testable package.
        case "$err" in *"build constraints exclude all Go files"*) continue ;; esac
        echo "doc-pinned-tests: go list ./$d failed: $err" >&2
        exit "$rc"
    fi
    pkgs+=("./$d")
done < <(printf '%s' "$dirs" | sort -u)

if [ "${#pkgs[@]}" -eq 0 ]; then
    [ "$list_only" -eq 1 ] || echo "doc-pinned-tests: no Go test pins a changed .md (${#md[@]} .md file(s) checked)"
    exit 0
fi

if [ "$list_only" -eq 1 ]; then
    printf '%s\n' "${pkgs[@]}"
    exit 0
fi

echo "doc-pinned-tests: ${#md[@]} .md file(s) changed; testing ${#pkgs[@]} package(s):"
printf '  %s\n' "${pkgs[@]}"
go test "${pkgs[@]}"
