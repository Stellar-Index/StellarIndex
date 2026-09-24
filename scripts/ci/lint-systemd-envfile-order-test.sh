#!/usr/bin/env bash
# lint-systemd-envfile-order-test — proves lint-systemd-envfile-order.sh
# fires on the SL07 shape (Environment= defaults after EnvironmentFile=),
# passes the corrected order, refuses a vacuous tree, and passes the
# repo's real supply-snapshot unit at HEAD.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-systemd-envfile-order.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

unit() {
  mkdir -p "$TMP/$1/configs/systemd"
  printf '%s\n' "$3" >"$TMP/$1/configs/systemd/$2"
}

expect() {
  bash "$LINT" "$TMP/$1" "configs/systemd/$2" >"$TMP/$1.out" 2>&1
  local rc=$?
  if [ "$rc" -eq "$3" ]; then ok "$4"; else bad "$4 (exit $rc, want $3)"; sed 's/^/      /' "$TMP/$1.out"; fi
}

unit bug x.service.j2 '[Service]
EnvironmentFile=-/etc/default/supply-snapshot
Environment=CONFIG_PATH=/etc/stellarindex.toml
Environment=ASSET=native'
expect bug x.service.j2 1 "SL07 shape (Environment= after EnvironmentFile=) is rejected"

unit fixed x.service.j2 '[Service]
Environment=CONFIG_PATH=/etc/stellarindex.toml
Environment=ASSET=native
EnvironmentFile=-/etc/default/supply-snapshot'
expect fixed x.service.j2 0 "defaults declared before EnvironmentFile= pass"

unit onlyfile x.service.j2 '[Service]
EnvironmentFile=-/etc/default/supply-snapshot'
expect onlyfile x.service.j2 0 "a unit with no Environment= default has nothing to override"

bash "$LINT" "$TMP/missing" "configs/systemd/nope.service.j2" >"$TMP/missing.out" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then ok "a missing file fails instead of passing vacuously"; else bad "missing file did not fail (exit $rc)"; fi

bash "$LINT" >"$TMP/repo.out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok "the real supply-snapshot unit at HEAD passes"; else bad "the real supply-snapshot unit fails"; sed 's/^/      /' "$TMP/repo.out"; fi

echo
echo "lint-systemd-envfile-order-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
