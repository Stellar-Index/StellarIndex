#!/usr/bin/env bash
# doc-pinned-tests-test.sh — fixture test for scripts/ci/doc-pinned-tests.sh.
# Run: bash scripts/ci/doc-pinned-tests-test.sh
set -uo pipefail

script="$(cd "$(dirname "$0")" && pwd)/doc-pinned-tests.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

check() { # check <name> <condition-as-test-args>...
    local name="$1"; shift
    if "$@"; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}

# shellcheck disable=SC2317,SC2329  # invoked indirectly via check
has() { case "$2" in *"$1"*) return 0 ;; esac; return 1; }
# shellcheck disable=SC2317,SC2329  # invoked indirectly via check
lacks() { ! has "$@"; }

# Subshell + unset: a hook-provided GIT_DIR must not point git at the real repo.
(
    unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
    cd "$tmp" || exit 1
    git init -q . && git config user.email t@t && git config user.name t
    printf 'module fx\n\ngo 1.21\n' > go.mod
    mkdir -p docs/protocols notes pinned other walker joiner
    echo one > docs/pinned.md; echo one > notes/free.md; echo one > docs/protocols/old.md
    printf 'package walker\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc TestWalks(t *testing.T) { _, _ = os.ReadDir("../docs/protocols") }\n' > walker/w_test.go
    printf 'package joiner\n\nimport (\n\t"os"\n\t"path/filepath"\n\t"testing"\n)\n\nfunc TestJoins(t *testing.T) { _, _ = os.ReadDir(filepath.Join("..", "docs")) }\n' > joiner/j_test.go
    cat > pinned/p_test.go <<'GO'
package pinned

import (
	"os"
	"testing"
)

func TestReadsDoc(t *testing.T) {
	b, err := os.ReadFile("../docs/pinned.md")
	if err != nil || string(b) != "one\n" {
		t.Fatal("docs/pinned.md changed")
	}
}
GO
    echo 'package other' > other/o.go
    git add . && git commit -qm base
    echo two > docs/pinned.md; echo two > notes/free.md
    git commit -qam change
    git mv docs/protocols/old.md docs/protocols/new.md
    git commit -qm rename
) >/dev/null 2>&1

run() { (unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE; cd "$tmp" && bash "$script" "$@" 2>&1); }

out="$(run --list HEAD~2)"
check "--list selects the package that pins a changed doc" has './pinned' "$out"
check "  and not a package that names no doc" lacks './other' "$out"

out="$(run --list --files notes/added.md)"
check "a doc no test names or walks selects nothing" [ -z "$out" ]

out="$(run --list --files docs/protocols/added.md)"
check "an added doc selects the test walking its directory" has './walker' "$out"
check "  and the test joining a bare \"docs\" segment" has './joiner' "$out"

out="$(run --list HEAD~1)"
check "a renamed doc selects the test walking its directory" has './walker' "$out"

out="$(run HEAD~2)"; rc=$?
check "a changed pinned string fails the run" [ "$rc" -ne 0 ]
check "  and names the broken doc" has 'docs/pinned.md changed' "$out"

out="$(run --files notes/free.md)"; rc=$?
check "an unpinned doc selects nothing and passes" [ "$rc" -eq 0 ]
check "  and says so" has 'no Go test pins' "$out"

out="$(run --list HEAD HEAD)"; rc=$?
check "no changed .md selects nothing" [ "$rc" -eq 0 -a -z "$out" ]

run --list nosuchref >/dev/null; rc=$?
check "a base that does not resolve fails" [ "$rc" -ne 0 ]

exit "$fail"
