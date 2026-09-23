#!/usr/bin/env bash
# lint-migrations-backnumber-test.sh — regression test for the GH-1164
# back-numbering guard in scripts/ci/lint-migrations.sh.
#
# The guard needs real git HISTORY (a comparison base vs HEAD) to tell a
# migration that is NEW in this change from one that has always been
# there, so it can't be exercised through lint-migrations-test.sh's
# temp-dir fixtures (those run against the real repo's migrations/ tree
# unconditionally — only the ClickHouse-dir portion is parameterized).
# This builds a throwaway git repo instead: a base commit with a gap
# in its migration numbering, then one commit that back-numbers a new
# migration INTO that gap (must be FATAL) and one that numbers a new
# migration past the head (must PASS).
#
# Run: bash scripts/ci/lint-migrations-backnumber-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
REPO_ROOT="$PWD"
TMP="$(mktemp -d)"
# Resolve symlinks (macOS: /var -> /private/var) so the fixture-isolation
# assertion below compares like with like — git itself resolves them via
# rev-parse --absolute-git-dir, and an unresolved $TMP never matches.
TMP="$(cd "$TMP" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0

indent() { printf '%s\n' "         ${1//$'\n'/$'\n'         }"; }

mkfixture() {
  local num="$1" name="$2"
  printf -- '-- fixture\nSELECT 1;\n' >"$TMP/migrations/${num}_${name}.up.sql"
  printf -- '-- fixture\nSELECT 1;\n' >"$TMP/migrations/${num}_${name}.down.sql"
}

register_row() {
  printf '| %s | fixture | x |\n' "$1" >>"$TMP/migrations/README.md"
}

# ─── build the scratch repo: base has 0001 and 0005, a gap at 0002-0004 ───
mkdir -p "$TMP/scripts/ci" "$TMP/migrations" "$TMP/deploy/clickhouse"
cp "$REPO_ROOT/scripts/ci/lint-migrations.sh" "$TMP/scripts/ci/lint-migrations.sh"
chmod +x "$TMP/scripts/ci/lint-migrations.sh"
printf 'CREATE TABLE IF NOT EXISTS stellar.dummy (id UInt64) ENGINE = MergeTree ORDER BY id;\n' \
  >"$TMP/deploy/clickhouse/dummy.sql"

mkfixture 0001 fixture
mkfixture 0005 fixture
{
  printf '# fixture register\n\n| Number | File | Adds |\n| --- | --- | --- |\n'
  register_row 0001
  register_row 0005
} >"$TMP/migrations/README.md"

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

git -C "$TMP" init -q -b main
case "$(git -C "$TMP" rev-parse --absolute-git-dir)" in
  "$TMP"/*) ;;
  *)
    echo "fixture escaped its directory — refusing to continue" >&2
    exit 1
    ;;
esac
git -C "$TMP" -c user.email=t@example.com -c user.name=t add -A
git -C "$TMP" -c user.email=t@example.com -c user.name=t commit -q -m base
base_sha="$(git -C "$TMP" rev-parse HEAD)"

# ─── case 1: back-numbered into the existing gap (0002) — FATAL ───
mkfixture 0002 backfilled
{
  printf '# fixture register\n\n| Number | File | Adds |\n| --- | --- | --- |\n'
  register_row 0001
  register_row 0002
  register_row 0005
} >"$TMP/migrations/README.md"
git -C "$TMP" add -A
git -C "$TMP" -c user.email=t@example.com -c user.name=t commit -q -m "back-numbered"

out="$(cd "$TMP" && BASE_SHA="$base_sha" ./scripts/ci/lint-migrations.sh 2>&1)"
got=$?
if [ "$got" -eq 0 ]; then
  echo "  FAIL back-numbered migration into an existing gap is fatal (exit 0, want non-zero)"
  fail=$((fail + 1))
else
  case "$out" in
    *"0002_backfilled.up.sql: new migration numbered 2 does not exceed"*) : ;;
    *)
      echo "  FAIL back-numbered migration into an existing gap is fatal (failed, but no matching finding)"
      indent "$out"
      fail=$((fail + 1))
      ;;
  esac
  if [ "$fail" -eq 0 ]; then
    echo "  ok   back-numbered migration into an existing gap is fatal (GH-1164)"
    pass=$((pass + 1))
  fi
fi

# ─── case 2: forward-numbered past the head (0006) — passes ───
git -C "$TMP" reset -q --hard "$base_sha"
mkfixture 0006 forward
{
  printf '# fixture register\n\n| Number | File | Adds |\n| --- | --- | --- |\n'
  register_row 0001
  register_row 0005
  register_row 0006
} >"$TMP/migrations/README.md"
git -C "$TMP" add -A
git -C "$TMP" -c user.email=t@example.com -c user.name=t commit -q -m "forward-numbered"

out="$(cd "$TMP" && BASE_SHA="$base_sha" ./scripts/ci/lint-migrations.sh 2>&1)"
got=$?
if [ "$got" -ne 0 ]; then
  echo "  FAIL a legitimate forward-numbered migration still passes (exit $got, want 0)"
  indent "$out"
  fail=$((fail + 1))
else
  echo "  ok   a legitimate forward-numbered migration still passes"
  pass=$((pass + 1))
fi

echo "lint-migrations-backnumber-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
