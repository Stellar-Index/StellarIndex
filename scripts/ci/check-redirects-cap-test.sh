#!/usr/bin/env bash
# check-redirects-cap-test.sh — fixture tests for
# scripts/ci/check-redirects-cap.sh (T335).
#
# Run: bash scripts/ci/check-redirects-cap-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-redirects-cap.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# gen_rules <n> — n distinct rule lines, no comments.
gen_rules() {
  local n="$1" i
  for ((i = 0; i < n; i++)); do
    printf '/r%d /dest%d 301\n' "$i" "$i"
  done
}

expect() {
  local name="$1" want_rc="$2"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# At the cap (100): OK.
gen_rules 100 > "$TMP/at-cap.txt"
OUT="$(bash "$CHECK" "$TMP/at-cap.txt" 100 2>&1)"; RC=$?
expect 'exactly 100 rules, cap 100 → OK' 0

# One over the cap: DRIFT. This is the regression T335 exists to catch —
# on the unfixed repo there is no script at all, so this is the proof
# the check actually fires past the cap rather than always passing.
gen_rules 101 > "$TMP/over-cap.txt"
OUT="$(bash "$CHECK" "$TMP/over-cap.txt" 100 2>&1)"; RC=$?
expect '101 rules, cap 100 → DRIFT' 1
if ! grep -q 'has 101 rules' <<<"$OUT"; then
  echo "FAIL: over-cap message missing rule count" >&2; fail=$((fail + 1))
else
  echo "ok: over-cap message names the count"; pass=$((pass + 1))
fi

# Comments and blank lines must not count toward the cap.
{
  echo "# a comment"
  echo ""
  gen_rules 5
  echo "# another comment describing the rules above"
} > "$TMP/with-comments.txt"
OUT="$(bash "$CHECK" "$TMP/with-comments.txt" 5 2>&1)"; RC=$?
expect '5 rules + comments/blanks, cap 5 → OK (comments not counted)' 0

# Missing file → hard failure, not a silent pass.
OUT="$(bash "$CHECK" "$TMP/does-not-exist.txt" 100 2>&1)"; RC=$?
expect 'missing file → error, not silent pass' 1

# The real file, against the real default cap: must currently pass.
OUT="$(bash "$CHECK" web/explorer/public/_redirects 100 2>&1)"; RC=$?
expect 'live web/explorer/public/_redirects is currently within cap' 0

echo
echo "check-redirects-cap-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
