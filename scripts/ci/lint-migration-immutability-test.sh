#!/usr/bin/env bash
# lint-migration-immutability-test.sh — fixture tests for the frozen-migration
# gate (scripts/ci/lint-migration-immutability.sh, W1-migrations-5).
#
# The gate is the only thing that turns a SILENT in-place edit of a shipped
# migration (golang-migrate never re-runs an applied version and never
# content-hashes — the 0138 incident) into a loud CI failure. A gate that
# stops tripping is worse than none, so its behaviour is pinned here rather
# than assumed:
#
#   - an in-place edit of a baselined migration is caught (the core case);
#   - a baselined migration deleted/renamed is caught;
#   - a NEW migration with no baseline entry is caught (append-only:
#     the checksum must be added in the same PR);
#   - a new migration added WITH its checksum (--write) passes;
#   - an unmodified tree passes.
#
# Fixtures are synthetic migrations dirs + a generated baseline under a
# tmpdir — no DB, no network, no repo state.
#
# Run: bash scripts/ci/lint-migration-immutability-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-migration-immutability.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

MIG="$TMP/migrations"
MANIFEST="$TMP/manifest"

pass=0
fail=0

reset()    { rm -rf "$MIG"; mkdir -p "$MIG"; }
put()      { printf '%s\n' "$2" > "$MIG/$1"; }               # put <name> <sql>
snapshot() { MIGRATIONS_DIR="$MIG" MIGRATION_IMMUTABILITY_MANIFEST="$MANIFEST" \
               bash "$LINT" --write >/dev/null 2>&1; }        # freeze current tree

run() {  # run the gate in check mode against the fixture tree + baseline
  OUT="$(MIGRATIONS_DIR="$MIG" MIGRATION_IMMUTABILITY_MANIFEST="$MANIFEST" \
           bash "$LINT" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> <want-substring-or-empty>
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# Baseline fixture: two migrations, frozen.
reset
put 0001_init.up.sql   'CREATE TABLE trades (id bigint);'
put 0001_init.down.sql 'DROP TABLE trades;'
snapshot

# 1. Unmodified tree → GREEN.
run
expect 'clean tree passes' 0 'passed'

# 2. In-place edit of an already-shipped migration → RED (the 0138 class).
put 0001_init.up.sql 'CREATE TABLE trades (id bigint, note text);'
run
expect 'in-place edit of a shipped migration is caught' 1 'content changed'
put 0001_init.up.sql 'CREATE TABLE trades (id bigint);'   # restore for isolation

# 3. New migration appended WITH its checksum (author ran --write) → GREEN.
put 0002_add.up.sql   'ALTER TABLE trades ADD COLUMN venue text;'
put 0002_add.down.sql 'ALTER TABLE trades DROP COLUMN venue;'
snapshot
run
expect 'new migration + refreshed baseline passes (append-only)' 0 'passed'

# 4. New migration WITHOUT updating the baseline → RED.
put 0003_more.up.sql   'ALTER TABLE trades ADD COLUMN book text;'
put 0003_more.down.sql 'ALTER TABLE trades DROP COLUMN book;'
run
expect 'new migration missing from baseline is caught' 1 'not in the checksum baseline'

# 5. A baselined migration deleted/renamed → RED.
reset
put 0001_init.up.sql   'CREATE TABLE trades (id bigint);'
put 0001_init.down.sql 'DROP TABLE trades;'
snapshot
rm "$MIG/0001_init.down.sql"
run
expect 'deleted/renamed shipped migration is caught' 1 'missing from tree'

# 6. --write is faithful: a freshly-written baseline verifies clean.
reset
put 0009_x.up.sql   'CREATE INDEX x ON t (a);'
put 0009_x.down.sql 'DROP INDEX x;'
snapshot
run
expect '--write output verifies clean' 0 'passed'

echo
echo "lint-migration-immutability-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
