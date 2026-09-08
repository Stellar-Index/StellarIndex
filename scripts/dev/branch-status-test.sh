#!/usr/bin/env bash
# Self-test for branch-status.sh. Builds a throwaway repository so the
# assertions are about the SCRIPT's logic, never about whatever branches
# this checkout happens to have today.
#
# The case that matters is the last one: a branch whose tip is NOT an
# ancestor of the base, which nonetheless deletes files the base has.
# That is the shape a three-dot diff cannot show you, and the shape that
# would have destroyed a day's work on 2026-09-08.
set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/branch-status.sh"
pass=0
fail=0

ok() { echo "  ok   $1"; pass=$((pass + 1)); }
no() { echo "  FAIL $1"; echo "       $2"; fail=$((fail + 1)); }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

git -C "$tmp" init -q -b main
git -C "$tmp" config user.email t@t
git -C "$tmp" config user.name t
echo one > "$tmp/a.txt"
git -C "$tmp" add a.txt
git -C "$tmp" commit -qm "base"

# forked BEFORE main grew b.txt, and deletes a.txt
git -C "$tmp" checkout -q -b stale
echo two > "$tmp/a.txt"
git -C "$tmp" commit -qam "stale edits a"
git -C "$tmp" rm -q a.txt
git -C "$tmp" commit -qm "stale deletes a"

git -C "$tmp" checkout -q main
echo new > "$tmp/b.txt"
git -C "$tmp" add b.txt
git -C "$tmp" commit -qm "main adds b"

# a branch whose tip IS already in main
git -C "$tmp" branch landed HEAD~1
# a branch with genuine unlanded work that deletes nothing
git -C "$tmp" checkout -q -b live
echo c > "$tmp/c.txt"
git -C "$tmp" add c.txt
git -C "$tmp" commit -qm "live adds c"
git -C "$tmp" checkout -q main

# `|| true`: a destructive branch exits 3 BY DESIGN, and under set -e a
# command substitution that fails would abort the harness before the very
# assertion this file exists for.
run() { ( cd "$tmp" && bash "$SCRIPT" "$@" 2>&1 ) || true; }
rc()  { ( cd "$tmp" && bash "$SCRIPT" "$@" >/dev/null 2>&1; echo $? ); }

out="$(run landed)"
case "$out" in *LANDED*) ok "a tip already in the base reads LANDED" ;;
    *) no "a tip already in the base reads LANDED" "$out" ;; esac
if [ "$(rc landed)" = 0 ]; then ok "LANDED exits 0"; else no "LANDED exits 0" "got $(rc landed)"; fi

out="$(run live)"
case "$out" in *"1 unlanded commit"*) ok "real unlanded work is counted" ;;
    *) no "real unlanded work is counted" "$out" ;; esac
if [ "$(rc live)" = 0 ]; then ok "unlanded-but-safe exits 0"; else no "unlanded-but-safe exits 0" "got $(rc live)"; fi

# THE ONE THAT MATTERS.
out="$(run stale)"
case "$out" in *"WOULD DELETE"*) ok "a stale branch that removes a base file is flagged" ;;
    *) no "a stale branch that removes a base file is flagged" "$out" ;; esac
case "$out" in *"would delete: a.txt"*) ok "the deleted path is named" ;;
    *) no "the deleted path is named" "$out" ;; esac
if [ "$(rc stale)" = 3 ]; then ok "a destructive branch exits 3"; else no "a destructive branch exits 3" "got $(rc stale)"; fi

# And the regression guard for the bug this script exists to prevent: the
# three-dot diff must NOT be what decides it. main...stale hides b.txt
# entirely, so a three-dot implementation cannot see the deletion of a file
# added to main after the fork. Prove the script sees what three-dot cannot.
three="$( cd "$tmp" && git diff --name-only main...stale )"
two="$( cd "$tmp" && git diff --name-only main stale )"
if [ "$three" != "$two" ]; then
    ok "the fixture really does distinguish two-dot from three-dot"
else
    no "the fixture really does distinguish two-dot from three-dot" "both: $two"
fi

# Match the BRANCH COLUMN, not the substring: "unlanded commit(s)" contains
# "landed", and an earlier version of this assertion failed on exactly that.
out="$(run --stale-only)"
if [[ "$out" =~ (^|$'\n')landed[[:space:]] ]]; then
    no "--stale-only hides LANDED branches" "$out"
else
    ok "--stale-only hides LANDED branches"
fi
# ...and it must still SHOW the branches that matter.
if [[ "$out" =~ (^|$'\n')stale[[:space:]] ]] && [[ "$out" =~ (^|$'\n')live[[:space:]] ]]; then
    ok "--stale-only still shows unlanded and destructive branches"
else
    no "--stale-only still shows unlanded and destructive branches" "$out"
fi

echo "branch-status-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
