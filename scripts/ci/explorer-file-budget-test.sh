#!/usr/bin/env bash
# explorer-file-budget-test.sh — fixture tests for the CF Pages file-count
# gate (scripts/ci/explorer-file-budget.sh).
#
# The gate had no floor: an empty $OUT (a build that emitted its HTML under
# a different path) made `count` 0, and the ceiling check passed vacuously —
# "OK (18500 files of headroom)" over a build that produced nothing.
# explorer-seo-lint.sh hit the identical incident shape on 2026-08-04 and
# was fixed with a minimum-file-count floor; this pins the same floor here.
#
# Run: bash scripts/ci/explorer-file-budget-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/explorer-file-budget.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# shellcheck source=scripts/ci/lib/test-expect.sh
. "$PWD/scripts/ci/lib/test-expect.sh"

# run <n-files> [limit] [margin] — a fixture $OUT with n files, then the gate.
run() {
  local n="$1" limit="${2:-}" margin="${3:-}"
  rm -rf "$TMP/out"
  mkdir -p "$TMP/out"
  local i=0
  while [ "$i" -lt "$n" ]; do
    : > "$TMP/out/page-$i.html"
    i=$((i + 1))
  done
  OUT="$(CF_PAGES_FILE_LIMIT="$limit" CF_PAGES_FILE_MARGIN="$margin" bash "$GATE" "$TMP/out" 2>&1)"
  RC=$?
}

# 1. The incident shape: an empty export must FAIL, not pass vacuously.
run 0
expect 'an empty export refuses to pass vacuously' 1 'Refusing to pass vacuously'

# 2. A healthy, ordinary count passes under the default ceiling.
run 100
expect 'a normal file count is OK' 0 'OK'

# 3. Over a (tightened, for the fixture) ceiling still fails.
run 9 10 2
expect 'a count over the ceiling fails' 1 'exceeds the'

# 4. Under the (tightened) ceiling still passes.
run 7 10 2
expect 'a count under the tightened ceiling is OK' 0 'OK'

# 5. A missing $OUT is the pre-existing build-not-run failure, unaffected
#    by the floor.
rm -rf "$TMP/missing"
OUT="$(bash "$GATE" "$TMP/missing" 2>&1)"; RC=$?
expect 'a missing out-dir still fails with the build-it-first message' 2 'build the explorer first'

echo
echo "explorer-file-budget-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
