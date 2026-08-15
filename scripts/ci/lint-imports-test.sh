#!/usr/bin/env bash
# lint-imports-test.sh — fixture tests for the import-boundary lint.
#
# Pins the gate's verdict to its inputs so a refactor can't silently
# weaken it (the same self-test discipline as lint-baseline-growth-test.sh
# and check-*-test.sh).
#
# The regression this file was added for (DEP-F2, audit-2026-08-14):
# the RULES ban matched banned packages by EXACT string
# (`if imp not in rule["banned"]`), so importing a SUBPACKAGE of a
# banned package — e.g. `.../protocols/horizon/operations` under banned
# `.../protocols/horizon` — slipped straight past the ban. A ban you
# evade by importing one directory deeper is not a ban. The fix is a
# prefix match (`imp == b or imp.startswith(b + "/")`), the exact
# boundary logic the LAYERING_RULES already use for module-local edges.
#
# Cases below pin all three edges of that match:
#   1. a SUBPACKAGE of a banned pkg is caught (the fix);
#   2. an EXACT banned import is still caught (no regression);
#   3. a mere name-prefix that is NOT a subpackage (`horizonfoo`) is
#      NOT a false hit (the `+ "/"` boundary — no over-match);
#   4. a subpackage of a banned pkg from a non-allowlisted prod file
#      is caught, but
#   5. the SAME subpackage from an allowlisted path (internal/scval/)
#      still passes — prefix-match composes with the allowlist; and
#   6. a clean tree passes.
#
# Run: bash scripts/ci/lint-imports-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/lint-imports.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# reset — rebuild the fixture tree as just a copy of the gate at its
# real relative path. The gate cds to `dirname $0/../..` and walks from
# there, so this layout is what points it at the fixture rather than at
# the real repo. No baseline file is written, so the gate runs strict.
reset() {
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo/scripts/ci"
  cp "$GATE" "$TMP/repo/scripts/ci/lint-imports.sh"
}

# addgo <rel-path> <import-path> — drop a minimal prod .go file that
# imports <import-path>. Only the import block is parsed (the lint never
# compiles), so an otherwise-empty file is enough.
addgo() {
  local rel="$1" imp="$2"
  mkdir -p "$TMP/repo/$(dirname "$rel")"
  {
    printf 'package p\n\n'
    printf 'import (\n\t"%s"\n)\n' "$imp"
  } > "$TMP/repo/$rel"
}

# runGate → sets RC + OUT
runGate() {
  OUT="$(cd "$TMP/repo" && bash scripts/ci/lint-imports.sh 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [want-substring]
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [[ "$RC" -ne "$want_rc" ]]; then
    echo "FAIL: $name — rc=$RC want=$want_rc"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  if [[ -n "$want_sub" && "$OUT" != *"$want_sub"* ]]; then
    echo "FAIL: $name — output missing: $want_sub"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# --- 1. THE FIX: a subpackage of a banned pkg is caught -------------
# Against the old exact-match gate this PASSED (silent evasion). It must
# now FAIL under C/no-horizon.
reset
addgo "internal/horizonleak/leak.go" "github.com/stellar/go/protocols/horizon/operations"
runGate
expect "horizon subpackage import is caught" 1 "C/no-horizon"

# --- 2. control: an exact banned import is still caught -------------
reset
addgo "internal/horizonleak/leak.go" "github.com/stellar/go/protocols/horizon"
runGate
expect "exact horizon import is still caught" 1 "C/no-horizon"

# --- 3. boundary: a name-prefix that is NOT a subpackage passes -----
# `horizonfoo` shares the string `horizon` as a prefix but is a
# different package (no `/` boundary) — the `+ "/"` guard must NOT flag
# it, or the ban would over-match unrelated packages.
reset
addgo "internal/horizonleak/leak.go" "github.com/stellar/go/protocols/horizonfoo"
runGate
expect "name-prefix lookalike is NOT a false hit" 0

# --- 4. xdr subpackage from a non-allowlisted prod file is caught ---
reset
addgo "internal/xdrleak/leak.go" "github.com/stellar/go-stellar-sdk/xdr/xdr3"
runGate
expect "xdr subpackage from prod file is caught" 1 "B/xdr-scoped-to-scval"

# --- 5. same xdr subpackage from an allowlisted path still passes ---
# Prefix-match must compose with the file allowlist: internal/scval/ is
# allowed to touch xdr, subpackages included.
reset
addgo "internal/scval/decode.go" "github.com/stellar/go-stellar-sdk/xdr/xdr3"
runGate
expect "xdr subpackage from allowlisted internal/scval passes" 0

# --- 6. a clean tree passes ----------------------------------------
reset
addgo "internal/fine/ok.go" "fmt"
runGate
expect "clean tree passes" 0

echo
echo "lint-imports-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
