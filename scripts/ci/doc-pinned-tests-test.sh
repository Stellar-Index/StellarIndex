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

# shellcheck disable=SC2329
has() { case "$2" in *"$1"*) return 0 ;; esac; return 1; }

# Subshell + unset: a hook-provided GIT_DIR must not point git at the real repo.
(
    unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
    cd "$tmp" || exit 1
    git init -q . && git config user.email t@t && git config user.name t
    printf 'module fx\n\ngo 1.21\n' > go.mod
    mkdir -p docs pinned other
    echo one > docs/pinned.md; echo one > docs/free.md
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
    echo two > docs/pinned.md; echo two > docs/free.md
    git commit -qam change
) >/dev/null 2>&1

run() { (unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE; cd "$tmp" && bash "$script" "$@" 2>&1); }

out="$(run --list HEAD~1)"
check "--list selects only the package that pins a changed doc" [ "$out" = "./pinned" ]

out="$(run HEAD~1)"; rc=$?
check "a changed pinned string fails the run" [ "$rc" -ne 0 ]
check "  and names the broken doc" has 'docs/pinned.md changed' "$out"

out="$(run --files docs/free.md)"; rc=$?
check "an unpinned doc selects nothing and passes" [ "$rc" -eq 0 ]
check "  and says so" has 'no Go test pins' "$out"

out="$(run --list HEAD HEAD)"; rc=$?
check "no changed .md selects nothing" [ "$rc" -eq 0 -a -z "$out" ]

exit "$fail"
