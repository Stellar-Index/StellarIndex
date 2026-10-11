#!/usr/bin/env bash
# A script that creates a git repository must first strip the GIT_* redirect
# variables it may have inherited.
#
# WHY. `git init` honours an inherited GIT_DIR AHEAD of its own `-C`. A git
# hook runs with GIT_DIR, GIT_INDEX_FILE and friends EXPORTED, and
# scripts/dev/lint-changed.sh (run by the pre-commit hook) dispatches changed
# `*-test.sh` files. A fixture's `git -C "$tmp" init` therefore ran against
# the REAL repository: core.bare was set on the live checkout and the
# fixture's commits landed on main. git printed only `warning: re-init` and
# the test passed.
#
# Reproduce, with a throwaway victim:
#
#     export GIT_DIR="$victim/.git"
#     git -C "$scratch" init -q     # creates NO $scratch/.git
#
# The rule: clear the redirect variables before the first `git … init`. A
# missed variable fails the same silent way, so a fixture SHOULD also assert
# that `git -C "$tmp" rev-parse --absolute-git-dir` lands under its own
# directory. This gate enforces only the mechanically checkable clearing and
# names the assertion in its failure text.
#
# Usage: lint-git-fixture-isolation.sh [file ...]   (default: scripts/)
# Exit 0 clean, 1 on an unguarded fixture, 2 on a usage error.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

if [ "$#" -gt 0 ]; then
    files=("$@")
else
    # No mapfile: bash 3.2 (macOS) lacks it, and a gate that dies locally only runs in CI.
    files=()
    while IFS= read -r line; do files+=("$line"); done < <(find scripts -name '*.sh' -type f | sort)
fi

if [ "${#files[@]}" -eq 0 ]; then
    echo "lint-git-fixture-isolation: no shell scripts in scope — refusing to pass vacuously" >&2
    exit 2
fi

problems=0
scanned=0

for f in "${files[@]}"; do
    [ -f "$f" ] || continue
    case "$f" in *.sh) ;; *) continue ;; esac
    scanned=$((scanned + 1))

    # First NON-COMMENT line that creates a repository (a `git init` in a
    # usage comment must not trip this). Forms matched: `git init -q .`,
    # `git -C "$d" init -q`, `git -C "$tmp" init -q -b main`; not matched:
    # `git commit`, `git config`.
    init_hits=$(grep -nE '^[^#]*[^-[:alnum:]_]?git[^|#]*[[:space:]]init([[:space:]]|$)' "$f" 2>/dev/null || true)
    init_line=${init_hits%%$'\n'*}
    init_line=${init_line%%:*}
    [ -z "$init_line" ] && continue

    # The clearing must come BEFORE the init; unsetting afterwards is too late.
    guard_hits=$(grep -nE '^[^#]*(unset[[:space:]]+[^#]*GIT_DIR|env[[:space:]]+-u[[:space:]]*GIT_DIR|GIT_DIR=)' "$f" 2>/dev/null || true)
    guard_line=${guard_hits%%$'\n'*}
    guard_line=${guard_line%%:*}

    if [ -z "$guard_line" ] || [ "$guard_line" -gt "$init_line" ]; then
        problems=$((problems + 1))
        echo "lint-git-fixture-isolation: $f:$init_line creates a git repository without first clearing GIT_DIR" >&2
        if [ -n "$guard_line" ]; then
            echo "    (a clearing exists at line $guard_line, but AFTER the init — the hijack has already happened)" >&2
        fi
    fi
done

if [ "$problems" -gt 0 ]; then
    cat >&2 <<'EOF'

Add this above the first `git … init`, then ASSERT it worked:

    unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
          GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

    git -C "$tmp" init -q
    case "$(git -C "$tmp" rev-parse --absolute-git-dir)" in
        "$tmp"/*) ;;
        *) echo "fixture escaped its directory — refusing to continue" >&2; exit 1 ;;
    esac

The assertion matters as much as the unset: miss one variable and the
failure is silent again. git says only "warning: re-init".
EOF
    echo "lint-git-fixture-isolation: FAIL — $problems unguarded fixture(s) across $scanned script(s)" >&2
    exit 1
fi

echo "lint-git-fixture-isolation: OK — $scanned script(s), every git fixture clears GIT_DIR first"
