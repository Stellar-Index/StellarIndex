#!/usr/bin/env bash
# migration-followup-gate-test.sh — fixture tests for
# scripts/ci/migration-followup-gate.sh against a throwaway git repo.
#
# Pins: a release that adds a migration with a `-- REQUIRED-FOLLOWUP:` line
# FAILS without the acknowledgement and names the command; the
# acknowledgement passes; a release with no marker passes; a skip-ahead
# baseline sees the skipped release's marker; a rollback passes; a marker
# in a down.sql or an already-shipped file is not re-asked; and an
# unresolvable version or baseline fails closed.
#
# Run: bash scripts/ci/migration-followup-gate-test.sh
set -uo pipefail

# A hook-run test inherits the caller's GIT_DIR, which would point every
# fixture `git` at the real repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/migration-followup-gate.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
REPO="$TMP/repo"
pass=0
fail=0

# commit_tag <tag> <file> <content> — write <file>, commit, tag.
commit_tag() {
  mkdir -p "$REPO/$(dirname "$2")"
  printf '%s\n' "$3" > "$REPO/$2"
  git -C "$REPO" add -- "$2" && git -C "$REPO" commit -qm "$1" && git -C "$REPO" tag "$1"
}

mkdir -p "$REPO"
git -C "$REPO" init -q . || exit 1
git -C "$REPO" config user.email t@t.invalid
git -C "$REPO" config user.name t
git -C "$REPO" config commit.gpgsign false
git -C "$REPO" config tag.gpgsign false
if [ "$(git -C "$REPO" rev-parse --show-toplevel)" != "$(cd "$REPO" && pwd -P)" ]; then
  echo "migration-followup-gate-test: fixture repo did not initialise in $REPO" >&2
  exit 2
fi
commit_tag v0.1.0 migrations/0001_base.up.sql 'CREATE TABLE cctp_events (id int);' || exit 1
commit_tag v0.2.0 migrations/0002_disarm.up.sql '-- REQUIRED-FOLLOWUP: stellarindex-ops projector-replay -source cctp -from 62146641 -write
BEGIN;
DELETE FROM cctp_events;
COMMIT;' || exit 1
commit_tag v0.3.0 migrations/0003_add.up.sql 'ALTER TABLE cctp_events ADD COLUMN x int;' || exit 1
commit_tag v0.4.0 migrations/0003_add.down.sql '-- REQUIRED-FOLLOWUP: never asked — deploys run ups only
ALTER TABLE cctp_events DROP COLUMN x;' || exit 1

# expect <desc> <want-exit> <needle-or-empty> <gate args...>
expect() {
  local desc="$1" want="$2" needle="$3" out got
  shift 3
  out="$(cd "$REPO" && GITHUB_STEP_SUMMARY=/dev/null bash "$GATE" "$@" 2>&1)"; got=$?
  if [ "$got" -eq "$want" ] && [[ -z "$needle" || "$out" == *"$needle"* ]]; then
    echo "  ok   $desc"; pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want${needle:+ naming: $needle})"
    printf '         %s\n' "${out//$'\n'/$'\n'         }"
    fail=$((fail + 1))
  fi
}

echo "migration-followup-gate-test"
expect "a release adding a marked migration fails without the acknowledgement and names the command" 1 \
  "0002_disarm.up.sql: stellarindex-ops projector-replay -source cctp -from 62146641 -write" v0.2.0 false
expect "the acknowledgement passes and still lists the command" 0 \
  "0002_disarm.up.sql: stellarindex-ops projector-replay" v0.2.0 true
expect "a release adding only an unmarked migration passes" 0 "declares a required follow-up" v0.3.0 false
expect "a skip-ahead deploy from the host's live version sees the skipped release's marker" 1 \
  "0002_disarm.up.sql" v0.3.0 false v0.1.0
expect "a rollback adds no migrations and passes" 0 "" v0.2.0 false v0.3.0
expect "a marker in a down.sql is not asked for" 0 "" v0.4.0 false
expect "a marker already shipped at the baseline is not asked again" 0 "" v0.4.0 false v0.2.0
expect "an unresolvable version fails closed" 1 "does not resolve" v9.9.9 true
expect "an unresolvable baseline fails closed" 1 "does not resolve" v0.2.0 true v9.9.9

echo "migration-followup-gate-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
