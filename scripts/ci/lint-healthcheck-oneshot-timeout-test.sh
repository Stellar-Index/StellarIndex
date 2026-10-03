#!/usr/bin/env bash
# lint-healthcheck-oneshot-timeout-test.sh — fixture tests for the
# oneshot start bound gate (scripts/ci/lint-healthcheck-oneshot-timeout.sh, T592).
#
# A Type=oneshot unit re-armed by a .timer that hangs with no
# TimeoutStartSec blocks every future scheduled run forever, invisibly;
# RuntimeMaxSec is ignored for oneshot and only adds journal noise.
# Pinned behaviour:
#
#   - a healthcheck oneshot unit without TimeoutStartSec is CAUGHT;
#   - a oneshot unit carrying RuntimeMaxSec is CAUGHT, in any scanned dir;
#   - a healthcheck oneshot unit with TimeoutStartSec alone passes;
#   - a non-oneshot unit is not required to carry it and may use RuntimeMaxSec;
#   - a directory with no unit files FAILS rather than passing vacuously;
#   - the repo's own unit files are clean.
#
# Run: bash scripts/ci/lint-healthcheck-oneshot-timeout-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-healthcheck-oneshot-timeout.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
check() { # check <desc> <want-exit> [lint args...]
  local desc="$1" want="$2" got
  shift 2
  bash "$LINT" "$@" >/dev/null 2>&1
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

mk runtimeonly runtimeonly.service '[Service]
Type=oneshot
RuntimeMaxSec=30s
ExecStart=/opt/stellarindex/healthchecks/runtimeonly.sh'
check "a oneshot unit bounded only by the ignored RuntimeMaxSec is caught" 1 "$TMP/runtimeonly"

mk both both.service '[Service]
Type=oneshot
TimeoutStartSec=30s
RuntimeMaxSec=30s
ExecStart=/opt/stellarindex/healthchecks/both.sh'
check "a oneshot unit carrying RuntimeMaxSec beside TimeoutStartSec is caught" 1 "$TMP/both"

mk good good.service '[Service]
Type=oneshot
TimeoutStartSec=30s
ExecStart=/opt/stellarindex/healthchecks/good.sh'
check "a oneshot unit with TimeoutStartSec passes" 0 "$TMP/good"

mk notoneshot simple.service '[Service]
Type=simple
RuntimeMaxSec=1h
ExecStart=/opt/stellarindex/healthchecks/daemon.sh'
check "a non-oneshot unit is exempt" 0 "$TMP/notoneshot"

mkdir -p "$TMP/empty"
check "a directory with no unit files FAILS rather than passing vacuously" 1 "$TMP/empty"

echo "lint-healthcheck-oneshot-timeout-test: extra unit dirs"

mk units/role/templates x.service.j2 '[Service]
Type=oneshot
TimeoutStartSec=10min
RuntimeMaxSec=10min'
check "a nested oneshot template carrying RuntimeMaxSec is caught" 1 "$TMP/good" "$TMP/units"

mk untimed untimed.service '[Service]
Type=oneshot
ExecStart=/usr/bin/true'
check "an extra-dir oneshot unit is not required to set TimeoutStartSec" 0 "$TMP/good" "$TMP/untimed"
check "an empty extra dir FAILS rather than passing vacuously" 1 "$TMP/good" "$TMP/empty"

echo "lint-healthcheck-oneshot-timeout-test: the real tree"
check "the repo's unit files are clean" 0

echo "lint-healthcheck-oneshot-timeout-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
