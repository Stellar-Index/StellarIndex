#!/usr/bin/env bash
# check-selftest-runners-test.sh — fixture tests for
# scripts/ci/check-selftest-runners.sh. Each case builds a synthetic
# scripts/ci + workflows + verify.sh + baseline tree; no repo state.
#
# Run: bash scripts/ci/check-selftest-runners-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-selftest-runners.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# setup <verify.sh body> <ci.yml body> <baseline body> — fresh fixture tree
# holding a-test.sh, b-test.sh and one non-test gate.
setup() {
  rm -rf "$TMP/f"
  mkdir -p "$TMP/f/ci" "$TMP/f/wf"
  touch "$TMP/f/ci/a-test.sh" "$TMP/f/ci/b-test.sh" "$TMP/f/ci/lint-a.sh"
  printf '%s\n' "$1" >"$TMP/f/verify.sh"
  printf '%s\n' "$2" >"$TMP/f/wf/ci.yml"
  printf '%s\n' "$3" >"$TMP/f/baseline"
}

# expect <name> <want-exit> [<stderr substring>]
expect() {
  local name="$1" want="$2" needle="${3:-}" got out
  out="$(CI_DIR="$TMP/f/ci" WORKFLOWS_DIR="$TMP/f/wf" VERIFY_SH="$TMP/f/verify.sh" \
    BASELINE="$TMP/f/baseline" bash "$CHECK" 2>&1)"
  got=$?
  if [ "$got" -ne "$want" ]; then
    echo "  FAIL $name: exit $got, want $want"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
  elif [ -n "$needle" ] && ! grep -qF -- "$needle" <<<"$out"; then
    echo "  FAIL $name: output lacks '$needle'"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
  else
    echo "  ok   $name"
    pass=$((pass + 1))
  fi
}

setup './scripts/ci/a-test.sh' 'run: ./scripts/ci/b-test.sh' ''
expect "every self-test has a runner" 0

setup './scripts/ci/a-test.sh' 'run: ./scripts/ci/lint-a.sh' ''
expect "an unlisted orphan fails and is named" 1 "b-test.sh is run by no workflow"

setup './scripts/ci/a-test.sh' 'run: ./scripts/ci/lint-a.sh' '# debt
b-test.sh  # trailing comment'
expect "a baselined orphan passes" 0

setup './scripts/ci/a-test.sh' 'run: ./scripts/ci/b-test.sh' 'b-test.sh'
expect "a baselined entry that now has a runner is stale" 1 "stale baseline entry b-test.sh"

setup './scripts/ci/a-test.sh' 'run: ./scripts/ci/b-test.sh' 'gone-test.sh'
expect "a baselined entry that no longer exists is stale" 1 "stale baseline entry gone-test.sh"

setup './scripts/ci/a-test.sh' 'for f in scripts/ci/b-test.sh; do git show base; done' ''
expect "a bare mention is not a runner" 1 "b-test.sh is run by no workflow"

setup 'bash scripts/ci/a-test.sh' 'run: ./scripts/ci/lint-a.sh' ''
printf 'run: ./scripts/ci/b-test.sh\n' >"$TMP/f/wf/other.yml"
expect "bash-prefixed and non-ci.yml workflow invocations count" 0

setup 'echo nothing' 'run: ./scripts/ci/lint-a.sh' ''
expect "no invocations at all fails instead of passing vacuously" 1 "must not pass vacuously"

echo "check-selftest-runners-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
