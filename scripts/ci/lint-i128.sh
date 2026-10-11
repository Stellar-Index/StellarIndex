#!/usr/bin/env bash
# i128 truncation grep-guard, Go side (ADR-0003).
#
# Fast zero-dependency first line of defence: reject `int64(<x>.Lo)` (also
# int32/16/8/int) — truncating a 128-bit Soroban value to its low word and
# discarding the high word. Correct: canonical.FromInt128Parts(int64(p.Hi),
# uint64(p.Lo)). `\bint(8|16|32|64)?\(` does NOT match `uint64(p.Lo)` (`\b`
# fails inside `uint`), and targeting only `.Lo` leaves the correct signed
# `int64(p.Hi)` alone. Narrowing of `.Hi` (and Int256 HiHi/LoLo) is left to
# the deep go/types guard.
#
# Siblings:
#   - internal/canonical/i128_truncation_guard_test.go — the DEEP guard: a
#     repo-wide go/types walk over every lossy conversion of the xdr 128/256-bit
#     words and every math/big Int64/Uint64/Float64 narrowing (//i128:ok escapes).
#   - scripts/ci/lint-migrations.sh — the SQL side (money columns must be
#     NUMERIC, with lint-money:ok escapes).
#
# Exit 0 clean, non-zero on any violation. Wired into `make verify`.
set -euo pipefail
cd "$(dirname "$0")/../.."
fail=0

# Production code only (tests exempt). Comment lines are skipped: docstrings
# mention int64(parts.Lo) to WARN against it.
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

# The same truncation one hop away: `lo := p.Lo`, then `int64(lo)` later in
# the file. File-local and flow-insensitive by design; the go/types guard is
# the authority on locals and this is its fast mirror.
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

# Widen-then-narrow in one expression (`new(big.Float).SetInt(x).Float64()`)
# leaves no parts word for the patterns above; the deep guard judges it by
# receiver type. `//i128:ok <reason>` on the line or the one above exempts it.
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
