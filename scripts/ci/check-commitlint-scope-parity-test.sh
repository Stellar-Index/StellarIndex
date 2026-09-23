#!/usr/bin/env bash
# check-commitlint-scope-parity-test.sh — regression test for
# scripts/ci/check-commitlint-scope-parity.sh: proves the repo's real
# commitlint.config.js scope-enum covers scopes that actually appear in
# main's commit history (e.g. `docs(changelog)`, `fix(explorer)`), and
# that a config missing those scopes fails the check.
#
# Run: bash scripts/ci/check-commitlint-scope-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-commitlint-scope-parity.sh"

pass=0
fail=0

expect() {
  local name="$1" want_rc="$2" got_rc="$3"
  if [ "$got_rc" -eq "$want_rc" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $name (want rc=$want_rc got rc=$got_rc)" >&2
  fi
}

# Range known to contain both docs(changelog) and fix(explorer) commits.
RANGE="ffa69c7a6..c81a28f72"

# Case 1: a config with 'changelog'/'explorer' missing from scope-enum
# must fail against a range that uses those scopes.
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
stale="$tmpdir/commitlint.config.js"
grep -v "'changelog'," "$PWD/commitlint.config.js" | grep -v "'explorer'," > "$stale"

bash "$CHECK" "$stale" "$RANGE" >/dev/null 2>&1
expect "stale config (missing scopes) fails against real history" 1 "$?"

# Case 2: the real, fixed config must pass against the same range.
bash "$CHECK" "$PWD/commitlint.config.js" "$RANGE" >/dev/null 2>&1
expect "fixed config passes against real history" 0 "$?"

echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
