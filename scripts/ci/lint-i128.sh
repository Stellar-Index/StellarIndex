#!/usr/bin/env bash
# i128 truncation grep-guard, Go side (ADR-0003).
#
# Fast zero-dependency first line of defence: reject `int64(<x>.Lo)` /
# `int32(<x>.Lo)` / `int16(<x>.Lo)` / `int8(<x>.Lo)` / `int(<x>.Lo)` —
# truncating a 128-bit Soroban value to its low word, discarding the
# high word (the classic KALIEN-class precision-loss bug); the narrower
# int32/16/8 conversions lose even more of the low word. The correct
# decode passes lo as uint64 to
# canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo)).
# `\bint(8|16|32|64)?\(` deliberately does NOT match the correct
# `uint64(p.Lo)` (the `\b` fails inside `uint`), and the pattern targets
# only `.Lo` so the correct signed high-word cast `int64(p.Hi)` is not
# flagged. Narrowing casts of `.Hi` (and the Int256 HiHi/LoLo words) are
# left to the deep go/types guard below.
#
# Siblings:
#   - internal/canonical/i128_truncation_guard_test.go — the DEEP
#     guard: a repo-wide go/types walk that catches every lossy
#     conversion shape of the xdr 128/256-bit part words (sign
#     reinterpretation, narrowing, floats) and every math/big
#     Int64/Uint64/Float64 narrowing, with //i128:ok escapes.
#   - scripts/ci/lint-migrations.sh — the SQL side (money columns
#     must be NUMERIC). The migration check that used to live here
#     moved there 2026-07-05 (broader name set + lint-money:ok
#     escapes).
#
# Exit 0 clean, non-zero on any violation. Wired into `make verify`.
set -euo pipefail
cd "$(dirname "$0")/../.."
fail=0

# i128 truncation in Go (production code; tests exempt). Skip comment
# lines (the ADR/decoder docstrings mention int64(parts.Lo) to WARN
# against it — that's not a violation).
hits=$(grep -rnE '\bint(8|16|32|64)?\([A-Za-z_][A-Za-z0-9_.]*\.Lo\)' \
  --include='*.go' internal/ cmd/ pkg/ 2>/dev/null \
  | grep -v '_test\.go' \
  | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || true)
if [ -n "$hits" ]; then
  echo "lint-i128 ❌ i128 truncation — int(8|16|32|64)(x.Lo) discards the high 64 bits (ADR-0003):" >&2
  echo "$hits" >&2
  echo "  → decode via canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo))." >&2
  fail=1
fi

# The same truncation one hop away: `lo := p.Lo` (or `=`) and then
# `int64(lo)` further down the same file. File-local and flow-
# insensitive by design — the go/types guard is the authority on locals;
# this is its fast mirror, so the grep gate and the deep gate reject the
# same rewrite of the same bug.
go_files=()
while IFS= read -r f; do go_files+=("$f"); done \
  < <(find internal cmd pkg -name '*.go' ! -name '*_test.go' 2>/dev/null | sort)
vhits=$(awk '
    FNR == 1 { split("", bound) }
    /^[[:space:]]*\/\// { next }
    {
      line = $0
      sub(/[[:space:]]\/\/.*$/, "", line)
      if (match(line, /[A-Za-z_][A-Za-z0-9_]*[[:space:]]*:?=[[:space:]]*[A-Za-z_][A-Za-z0-9_.]*\.Lo([^A-Za-z0-9_]|$)/)) {
        v = substr(line, RSTART, RLENGTH); sub(/[[:space:]]*:?=.*$/, "", v); bound[v] = 1
      }
      for (v in bound) {
        if (match(line, "(^|[^A-Za-z0-9_])int(8|16|32|64)?\\(" v "\\)")) print FILENAME ":" FNR ":" $0
      }
    }' "${go_files[@]}" || true)
if [ -n "$vhits" ]; then
  echo "lint-i128 ❌ i128 truncation through a local — the variable was bound to <x>.Lo earlier in the file (ADR-0003):" >&2
  echo "$vhits" >&2
  echo "  → pass the word as uint64(p.Lo) to canonical.FromInt128Parts; do not narrow it first." >&2
  fail=1
fi

# Widen-then-narrow in one expression: `new(big.Float).SetInt(x).Float64()`
# leaves no parts word for the patterns above. The go/types guard judges
# every math/big Int64/Uint64/Float64 by receiver type; this catches the
# inline spelling. A `//i128:ok <reason>` on the line or the one above
# exempts it, matching the deep guard.
bhits=$(awk '
    FNR == 1 { prev = "" }
    /^[[:space:]]*\/\// { prev = $0; next }
    {
      ok = ($0 ~ /\/\/[[:space:]]*i128:ok[[:space:]]+[^[:space:]]/) || (prev ~ /\/\/[[:space:]]*i128:ok[[:space:]]+[^[:space:]]/)
      if (!ok && $0 ~ /new\(big\.(Int|Rat|Float)\).*\.(Int64|Uint64|Float64)\(\)/) print FILENAME ":" FNR ":" $0
      prev = $0
    }' "${go_files[@]}" || true)
if [ -n "$bhits" ]; then
  echo "lint-i128 ❌ math/big value narrowed to a machine number (ADR-0003):" >&2
  echo "$bhits" >&2
  echo "  → keep the amount in *big.Int / *big.Rat and render with FloatString, or mark //i128:ok <reason> if it is not an amount." >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "✅ i128 lint passed."
fi
exit "$fail"
