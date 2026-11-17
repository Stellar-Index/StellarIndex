#!/usr/bin/env bash
# lint-i128-test.sh — fixture tests for scripts/ci/lint-i128.sh.
#
# Pins the //i128:ok marker scope: a marker on the same line, or alone on the
# line above, exempts a math/big narrowing; a marker trailing an unrelated
# code line must not exempt the line below it.
#
# Run: bash scripts/ci/lint-i128-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-i128.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
# run_case <desc> <want-exit> <go source>
run_case() {
  local desc="$1" want="$2" src="$3" root got
  root="$(mktemp -d "$TMP/case.XXXXXX")"
  mkdir -p "$root/scripts/ci" "$root/internal/x" "$root/cmd" "$root/pkg"
  cp "$LINT" "$root/scripts/ci/lint-i128.sh"
  printf '%s\n' "$src" >"$root/internal/x/x.go"
  bash "$root/scripts/ci/lint-i128.sh" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (want exit $want, got $got)"
    fail=$((fail + 1))
  fi
}

run_case "unmarked narrowing is flagged" 1 \
  'a, _ := new(big.Rat).SetFrac(x, y).Float64()'
run_case "same-line marker exempts" 0 \
  'a, _ := new(big.Rat).SetFrac(x, y).Float64() // i128:ok a share'
run_case "marker alone on the line above exempts" 0 \
  '// i128:ok a share
a, _ := new(big.Rat).SetFrac(x, y).Float64()'
run_case "marker trailing a code line does not exempt the next line" 1 \
  'b, _ := new(big.Rat).SetFrac(x, y).Float64() // i128:ok a share
a, _ := new(big.Rat).SetFrac(x, y).Float64()'

echo "lint-i128-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
