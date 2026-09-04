#!/usr/bin/env bash
# lint-external-channels-test.sh — prove the channel gate can fail, and
# prove the revert path it promises actually works. A gate nobody has
# seen red is not a gate.
#
# The forbidden strings are read OUT of the manifest at runtime rather
# than written here, because a test that hard-coded them would itself be
# a file containing a reference to a switched-off channel — it would
# trip the very gate it exercises, and the only way out would be a
# second exemption. Reading them keeps the exemption list at two.
#
# The fixture doc is deliberately UNTRACKED and the git index is never
# touched: the gate reads untracked, non-ignored files, so a plain file
# is visible to it (same reasoning as lint-doc-links-test.sh).
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-external-channels.sh"
REAL_MANIFEST="docs/operations/external-channels.md"
FIX="docs/zz-external-channels-fixture.md"
FIXTURE_PAT="zz-dead-channel-fixture-string"
TMP=$(mktemp -d)
PASS=0; FAIL=0
# shellcheck disable=SC2329  # invoked indirectly by the EXIT trap
cleanup() { rm -f "$FIX"; rm -rf "$TMP"; }
trap cleanup EXIT

check() { # <name> <expected ok|red> [manifest]
  local name="$1" want="$2" rc
  if [ $# -ge 3 ]; then
    CHANNELS_MANIFEST="$3" bash "$GATE" >/dev/null 2>&1; rc=$?
  else
    bash "$GATE" >/dev/null 2>&1; rc=$?
  fi
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

write_manifest() { # write_manifest <path> <state> <patterns>
  { printf '# fixture manifest\n\n'
    printf '| id | state | where | strings forbidden while disabled |\n'
    printf '|---|---|---|---|\n'
    # shellcheck disable=SC2016  # the backticks are literal markdown, not a subshell
    printf '| `zz-fixture` | `%s` | fixture | %s |\n' "$2" "$3"
  } > "$1"
}

# The first forbidden string of the first disabled channel in the REAL
# manifest — what a shipped file must not say today.
REAL_PAT=$(awk -F'|' '
  /^\| *`[^`]+` *\|/ {
    state = $3; gsub(/[` ]/, "", state)
    if (state != "disabled") next
    split($5, parts, ",")
    p = parts[1]; sub(/^[ \t]*`/, "", p); sub(/`[ \t]*$/, "", p)
    if (p != "") { print p; exit }
  }' "$REAL_MANIFEST")

if [ -z "$REAL_PAT" ]; then
  echo "  SKIP no channel in $REAL_MANIFEST is currently disabled —"
  echo "       nothing to forbid, so the shipped-manifest cases cannot run."
fi

check "clean tree passes" ok

if [ -n "$REAL_PAT" ]; then
  printf '# fixture\n\nSee %s for details.\n' "$REAL_PAT" > "$FIX"
  check "a file referencing a switched-off channel is caught" red
  rm -f "$FIX"
fi

write_manifest "$TMP/disabled.md" disabled "\`$FIXTURE_PAT\`"
write_manifest "$TMP/enabled.md" enabled "\`$FIXTURE_PAT\`"
printf '# fixture\n\nSee %s for details.\n' "$FIXTURE_PAT" > "$FIX"
check "a disabled channel's string is caught" red "$TMP/disabled.md"

# The revert this gate promises: one row flipped, same reference, green.
check "the same string passes once the channel is enabled" ok "$TMP/enabled.md"
rm -f "$FIX"

check "a missing manifest fails rather than passing vacuously" red "$TMP/does-not-exist.md"

printf '# fixture manifest with no rows\n' > "$TMP/norows.md"
check "a manifest with zero channel rows fails" red "$TMP/norows.md"

write_manifest "$TMP/bogus.md" "swiched-on" "\`$FIXTURE_PAT\`"
check "an unrecognised state fails instead of meaning 'not disabled'" red "$TMP/bogus.md"

# A whitespace-only cell used to yield a pattern of literal spaces,
# which matched almost every line in the tree instead of failing.
write_manifest "$TMP/nopat.md" disabled " "
check "a disabled channel naming no string fails" red "$TMP/nopat.md"

# shellcheck disable=SC2016  # the backticks are literal markdown, not a subshell
write_manifest "$TMP/short.md" disabled '`io`'
check "a forbidden string too short to name a channel fails" red "$TMP/short.md"

check "clean tree passes again" ok

echo
echo "lint-external-channels-test: $PASS passed, $FAIL failed"
exit "$FAIL"
