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
#     reinterpretation, narrowing, floats), with //i128:ok escapes.
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

if [ "$fail" -eq 0 ]; then
  echo "✅ i128 lint passed."
fi
exit "$fail"
