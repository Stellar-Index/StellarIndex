#!/usr/bin/env bash
# lint-decimals-default-test.sh — fixture tests for scripts/ci/lint-decimals-default.sh.
#
# Pins what counts as defaulting a token scale to 7, the decimals-7 marker's
# scope (same line, or a comment line directly above; not a trailing marker on
# the previous code line), the constant-declaration exemption and the stale
# marker failure.
#
# Run: bash scripts/ci/lint-decimals-default-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-decimals-default.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
# run_case <desc> <want-exit> <go source>
run_case() {
  local desc="$1" want="$2" src="$3" root got
  root="$(mktemp -d "$TMP/case.XXXXXX")"
  mkdir -p "$root/scripts/ci" "$root/internal/x" "$root/cmd"
  cp "$LINT" "$root/scripts/ci/lint-decimals-default.sh"
  printf '%s\n' "$src" >"$root/internal/x/x.go"
  bash "$root/scripts/ci/lint-decimals-default.sh" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (want exit $want, got $got)"
    fail=$((fail + 1))
  fi
}

run_case "struct field set to 7 is flagged" 1 \
  'func f() T { return T{Decimals: 7} }'
run_case "field set to 7 on its own line is flagged" 1 \
  'func f() T {
	return T{
		Decimals:   7,
	}
}'
run_case "variable assigned 7 is flagged" 1 \
  'func f() { baseDecimals := 7; _ = baseDecimals }'
run_case "field assigned defaultTokenDecimals is flagged" 1 \
  'func f(r *R) { r.Decimals = defaultTokenDecimals }'
run_case "field assigned aggregate.StandardDecimals is flagged" 1 \
  'func f(r *R) { r.QuoteDecimals = aggregate.StandardDecimals }'
run_case "a decimals resolver returning 7 is flagged" 1 \
  'func (s *Server) tokenDecimals(id string) (int, bool) {
	if id == "" {
		return 7, true
	}
	return 0, false
}'
run_case "a decimals resolver returning defaultTokenDecimals is flagged" 1 \
  'func assetDecimals() int {
	return defaultTokenDecimals
}'
run_case "same-line marker exempts" 0 \
  'func f() T { return T{Decimals: 7} } // decimals-7: native, fixed by the protocol'
run_case "marker alone on the line above exempts" 0 \
  'func f() T {
	return T{
		// decimals-7: classic, fixed by the protocol
		Decimals: 7,
	}
}'
run_case "marker trailing a code line does not exempt the next line" 1 \
  'func f() (T, T) {
	a := T{Decimals: 7} // decimals-7: classic
	b := T{Decimals: 7}
	return a, b
}'
run_case "marker without a reason does not exempt" 1 \
  'func f() T { return T{Decimals: 7} } // decimals-7:'
run_case "marker on a line that no longer sets the scale is stale" 1 \
  'func f() T { return T{Decimals: d} } // decimals-7: classic'
run_case "constant declarations are exempt" 0 \
  'const defaultTokenDecimals = 7
const (
	StandardDecimals = 7
)'
run_case "a comparison against 7 is not a default" 0 \
  'func f(d int) bool { return d == 7 || d != 7 }'
run_case "returning 7 from a function not named for decimals is not flagged" 0 \
  'func days() int { return 7 }'
run_case "a different scale is not flagged" 0 \
  'func f() T { return T{Decimals: 18, Places: 7} }'

echo "lint-decimals-default-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
