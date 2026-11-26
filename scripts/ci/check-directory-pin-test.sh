#!/usr/bin/env bash
# check-directory-pin-test.sh — fixture tests for scripts/ci/check-directory-pin.sh,
# the decision core of directory-pin-reminder.yml. No network.
#
# Run: bash scripts/ci/check-directory-pin-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-directory-pin.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

PIN=b78cf196f79a73daed6cc1bc4166001cef98d86f
HEAD_SHA=0123456789abcdef0123456789abcdef01234567

write_defaults() {
  printf 'x: 1\ndirectory_sync_url: "%s"\ndirectory_sync_sha256: "00"\n' "$1" > "$TMP/defaults.yml"
}
commit_json() { printf '{"sha":"%s","commit":{"committer":{"date":"%s"}}}' "$1" "$2" > "$3"; }

# run <pin-date> <head-date> <ahead_by>
run() {
  commit_json "$PIN" "$1" "$TMP/pin.json"
  commit_json "$HEAD_SHA" "$2" "$TMP/head.json"
  printf '{"ahead_by":%s}' "$3" > "$TMP/cmp.json"
  out="$(DEFAULTS_FILE="$TMP/defaults.yml" PIN_COMMIT_JSON="$TMP/pin.json" HEAD_COMMIT_JSON="$TMP/head.json" \
    COMPARE_JSON="$TMP/cmp.json" BODY_OUT="$TMP/body.md" bash "$CHECK" verdict 2>&1)"
  rc=$?
}

expect() { # <name> <rc> <substring>
  if [ "$rc" = "$2" ] && grep -qF -- "$3" <<<"$out"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $1 (rc=$rc, want $2, substring '$3')"
    echo "$out"
  fi
}

write_defaults "https://github.com/stellar-expert/public-directory/archive/${PIN}.tar.gz"

out="$(DEFAULTS_FILE="$TMP/defaults.yml" bash "$CHECK" parse 2>&1)"; rc=$?
expect 'parse repo' 0 'repo=stellar-expert/public-directory'
expect 'parse pin' 0 "pin=${PIN}"

rm -f "$TMP/body.md"
run 2026-01-01T00:00:00Z 2026-01-23T00:00:00Z 5
expect '22 days behind and ahead -> STALE' 0 'verdict=STALE'
expect '22 days reported' 0 'days_behind=22'
body="$(cat "$TMP/body.md" 2>/dev/null || true)"
for want in "$PIN" "$HEAD_SHA" 'shasum -a 256' 'scam-flagged' 'directory_sync_sha256' \
  "archive/${HEAD_SHA}.tar.gz" 'api.md#stellarindex_directory_sync_snapshot_stale'; do
  if grep -qF -- "$want" <<<"$body"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: body lacks '$want'"; fi
done

run 2026-01-01T00:00:00Z 2026-01-22T00:00:00Z 5
expect 'exactly 21 days -> CURRENT (strictly more than)' 0 'verdict=CURRENT'

run 2026-01-01T00:00:00Z 2026-03-01T00:00:00Z 0
expect 'old pin but upstream has no newer commits -> CURRENT' 0 'verdict=CURRENT'

rm -f "$TMP/body.md"
run 2026-01-01T00:00:00Z 2026-01-10T00:00:00Z 3
expect 'fresh pin -> CURRENT' 0 'verdict=CURRENT'
if [ -e "$TMP/body.md" ]; then fail=$((fail + 1)); echo "FAIL: body written for CURRENT"; else pass=$((pass + 1)); fi

write_defaults "https://github.com/stellar-expert/public-directory/archive/refs/heads/master.tar.gz"
run 2026-01-01T00:00:00Z 2026-03-01T00:00:00Z 3
expect 'branch pin rejected' 2 'not pinned to a 40-hex commit'

echo "check-directory-pin-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
