#!/usr/bin/env bash
# lint-migration-immutability-test.sh — fixture tests for the frozen-migration
# gate (scripts/ci/lint-migration-immutability.sh).
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

# shellcheck source=scripts/ci/lib/test-expect.sh
. "$PWD/scripts/ci/lib/test-expect.sh"

reset()    { rm -rf "$MIG"; mkdir -p "$MIG"; }
put()      { printf '%s\n' "$2" > "$MIG/$1"; }               # put <name> <sql>
snapshot() { MIGRATIONS_DIR="$MIG" MIGRATION_IMMUTABILITY_MANIFEST="$MANIFEST" \
               bash "$LINT" --write >/dev/null 2>&1; }        # freeze current tree

run() {  # run the gate in check mode against the fixture tree + baseline
  OUT="$(MIGRATIONS_DIR="$MIG" MIGRATION_IMMUTABILITY_MANIFEST="$MANIFEST" \
           bash "$LINT" 2>&1)"
  RC=$?
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

# 7. Against a base commit (the deploy's host-live release): the manifest is
#    ignored, so an edit that re-blessed its own manifest is still caught.
#    The fixture is its own git repo; a hook's GIT_* must not leak into it.
REPO="$TMP/repo"
g() { env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_COMMON_DIR -u GIT_PREFIX \
  git -C "$REPO" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "$@"; }
runbase() {
  OUT="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_COMMON_DIR -u GIT_PREFIX \
    MIGRATIONS_DIR="$REPO/migrations" MIGRATION_IMMUTABILITY_MANIFEST=/nonexistent \
    MIGRATION_IMMUTABILITY_BASE="$1" bash "$LINT" 2>&1)"
  RC=$?
}
mkdir -p "$REPO/migrations"
printf '%s\n' '-- header' 'CREATE TABLE t (a int);' > "$REPO/migrations/0001_t.up.sql"
printf '%s\n' 'DROP TABLE t;' > "$REPO/migrations/0001_t.down.sql"
g init -q . && g add . && g commit -q -m base
base="$(g rev-parse HEAD)"
runbase "$base"
expect 'base mode: unchanged tree passes' 0 '1 shipped up migration'
printf '%s\n' '-- header, corrected' 'CREATE TABLE t (a int);' > "$REPO/migrations/0001_t.up.sql"
printf '%s\n' 'DROP TABLE IF EXISTS t;' > "$REPO/migrations/0001_t.down.sql"
printf '%s\n' 'CREATE TABLE u (a int);' > "$REPO/migrations/0002_u.up.sql"
runbase "$base"
expect 'base mode: header comment, down body and a new migration pass' 0 'unchanged'
printf '%s\n' '-- header' 'CREATE TABLE t (a int);' 'UPDATE t SET a = 0;' > "$REPO/migrations/0001_t.up.sql"
runbase "$base"
expect 'base mode: an edited shipped up body FAILS' 1 'differs from'
rm "$REPO/migrations/0001_t.up.sql"
runbase "$base"
expect 'base mode: a deleted shipped up FAILS' 1 'missing here'
runbase "0000000000000000000000000000000000000000"
expect 'base mode: an unreadable base FAILS closed' 1 'fail-closed'

# 8. Rollback: the tree is an older release than BASE, so BASE's newer
#    migration is absent. Passes only when the deploy skips migrations.
g checkout -q -- migrations && g clean -qfd migrations
printf '%s\n' 'CREATE TABLE v (a int);' > "$REPO/migrations/0002_v.up.sql"
g add . && g commit -q -m newer
newer="$(g rev-parse HEAD)"
g checkout -q "$base"
runbase "$newer"
expect 'base mode: rollback past a newer migration without migrations_skip FAILS' 1 'migrations_skip=true'
MIGRATION_IMMUTABILITY_MIGRATIONS_SKIPPED=true runbase "$newer"
expect 'base mode: rollback with migrations skipped passes' 0 'unchanged'
MIGRATION_IMMUTABILITY_MIGRATIONS_SKIPPED=True runbase "$newer"
expect 'base mode: a skip flag that is not exactly "true" takes the strict path' 1 'migrations_skip=true'
printf '%s\n' 'CREATE TABLE w (a int);' > "$REPO/migrations/0003_w.up.sql"
g add . && g commit -q -m w
mid="$(g rev-parse HEAD)"
printf '%s\n' 'CREATE TABLE v (a int);' > "$REPO/migrations/0002_v.up.sql"
g add . && g commit -q -m late
late="$(g rev-parse HEAD)"
g checkout -q "$mid"
MIGRATION_IMMUTABILITY_MIGRATIONS_SKIPPED=true runbase "$late"
expect 'base mode: a gap below the tree'"'"'s newest migration FAILS under skip' 1 'missing here'
g checkout -q "$newer"
rm "$REPO/migrations/0002_v.up.sql"
MIGRATION_IMMUTABILITY_MIGRATIONS_SKIPPED=true runbase "$newer"
expect 'base mode: deleting the newest shipped up is not a rollback and FAILS' 1 'missing here'

# 9. A hotfix cut from an older release tag is a sibling of BASE, not its
#    ancestor: the same rollback rules apply.
g checkout -q -- migrations && g checkout -q "$base"
printf 'fix\n' > "$REPO/HOTFIX" && g add HOTFIX && g commit -q -m hotfix
MIGRATION_IMMUTABILITY_MIGRATIONS_SKIPPED=true runbase "$newer"
expect 'base mode: a hotfix sibling of BASE with migrations skipped passes' 0 'migrations are skipped'
runbase "$newer"
expect 'base mode: a hotfix sibling of BASE without migrations_skip FAILS' 1 'migrations_skip=true'
g rm -q migrations/0001_t.up.sql && g commit -q -m drop
MIGRATION_IMMUTABILITY_MIGRATIONS_SKIPPED=true runbase "$newer"
expect 'base mode: a hotfix that deletes a shipped up still FAILS' 1 'missing here'

echo
echo "lint-migration-immutability-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
