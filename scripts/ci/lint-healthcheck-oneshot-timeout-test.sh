#!/usr/bin/env bash
# lint-healthcheck-oneshot-timeout-test.sh — fixture tests for the
# oneshot start/runtime bound gate (scripts/ci/lint-healthcheck-oneshot-timeout.sh, T592).
#
# A Type=oneshot unit re-armed by a .timer that hangs with no
# TimeoutStartSec/RuntimeMaxSec blocks every future scheduled run
# forever, invisibly. Pinned behaviour:
#
#   - a oneshot unit missing either directive is CAUGHT;
#   - a oneshot unit carrying both passes;
#   - a non-oneshot unit is not required to carry them;
#   - a directory with no unit files FAILS rather than passing vacuously;
#   - the repo's own configs/healthchecks/ is clean.
#
# Run: bash scripts/ci/lint-healthcheck-oneshot-timeout-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-healthcheck-oneshot-timeout.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
check() { # check <desc> <want-exit> <dir>
  local desc="$1" want="$2" dir="$3" got
  bash "$LINT" "$dir" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then echo "  ok   $desc"; pass=$((pass + 1))
  else echo "  FAIL $desc (exit $got, want $want)"; fail=$((fail + 1)); fi
}
mk() { mkdir -p "$TMP/$1"; printf '%s\n' "$3" > "$TMP/$1/$2"; }

echo "lint-healthcheck-oneshot-timeout-test: detection"

mk bad bad.service '[Service]
Type=oneshot
ExecStart=/opt/stellarindex/healthchecks/bad.sh'
check "a oneshot unit with neither directive is caught" 1 "$TMP/bad"

mk half half.service '[Service]
Type=oneshot
TimeoutStartSec=30s
ExecStart=/opt/stellarindex/healthchecks/half.sh'
check "a oneshot unit missing RuntimeMaxSec is caught" 1 "$TMP/half"

mk good good.service '[Service]
Type=oneshot
TimeoutStartSec=30s
RuntimeMaxSec=30s
ExecStart=/opt/stellarindex/healthchecks/good.sh'
check "a oneshot unit with both directives passes" 0 "$TMP/good"

mk notoneshot simple.service '[Service]
Type=simple
ExecStart=/opt/stellarindex/healthchecks/daemon.sh'
check "a non-oneshot unit is exempt" 0 "$TMP/notoneshot"

mkdir -p "$TMP/empty"
check "a directory with no unit files FAILS rather than passing vacuously" 1 "$TMP/empty"

echo "lint-healthcheck-oneshot-timeout-test: the real tree"
check "configs/healthchecks/ is clean" 0 "configs/healthchecks"

echo "lint-healthcheck-oneshot-timeout-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
