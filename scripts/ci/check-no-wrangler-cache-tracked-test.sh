#!/usr/bin/env bash
# check-no-wrangler-cache-tracked-test.sh — fixture tests for
# scripts/ci/check-no-wrangler-cache-tracked.sh (T535).
#
# Builds a throwaway git repo per case so the check's `git ls-files` /
# `git check-ignore` calls exercise real git behaviour, not a mock.
#
# Run: bash scripts/ci/check-no-wrangler-cache-tracked-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-no-wrangler-cache-tracked.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

expect() {
  local name="$1" want_rc="$2" got_rc="$3"
  if [ "$got_rc" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $got_rc, want $want_rc" >&2
    fail=$((fail + 1))
  else
    pass=$((pass + 1))
  fi
}

# mkrepo <name> <gitignore-line> <track-cache-file: yes|no>
mkrepo() {
  local dir="$TMP/$1"
  mkdir -p "$dir/docs/reference/api/.wrangler/cache"
  (
    unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
          GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR
    cd "$dir" || exit 1
    real_dir="$(pwd -P)"
    git init -q
    case "$(git rev-parse --absolute-git-dir)" in
      "$real_dir"/*) ;;
      *) echo "fixture escaped its directory — refusing to continue" >&2; exit 1 ;;
    esac
    git config user.email test@example.com
    git config user.name test
    printf '%s\n' "$2" > .gitignore
    echo '{"account_id":"deadbeef00000000000000000000000"}' \
      > docs/reference/api/.wrangler/cache/pages.json
    git add .gitignore
    if [ "$3" = "yes" ]; then
      git add -f docs/reference/api/.wrangler/cache/pages.json
    fi
    git commit -q -m init
  )
}

# Case 1: pre-fix state — root-anchored ignore, cache file tracked. RED.
mkrepo "unfixed" "/.wrangler/" "yes"
bash "$CHECK" "$TMP/unfixed"
expect "unfixed tree fails" 1 $?

# Case 2: ignore fixed but file still tracked from before — still RED
# (tracked files aren't retroactively ignored; this is the untrack step).
mkrepo "still-tracked" "**/.wrangler/" "yes"
bash "$CHECK" "$TMP/still-tracked"
expect "tracked-despite-ignore fails" 1 $?

# Case 3: post-fix state — broadened ignore, file untracked. GREEN.
mkrepo "fixed" "**/.wrangler/" "no"
bash "$CHECK" "$TMP/fixed"
expect "fixed tree passes" 0 $?

echo "check-no-wrangler-cache-tracked-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
