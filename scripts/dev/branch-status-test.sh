#!/usr/bin/env bash
# Self-test for branch-status.sh. Builds a throwaway repository so the
# assertions are about the SCRIPT's logic, never about whatever branches
# this checkout happens to have today.
#
# The case that matters is the last one: a branch whose tip is NOT an
# ancestor of the base, which nonetheless deletes files the base has.
# That is the shape a three-dot diff cannot show you, and the shape that
# would have destroyed a day's work on 2026-09-08.
#
# A FIXTURE IS ONLY THROWAWAY IF GIT AGREES (2026-09-09). `scripts/dev/
# lint-changed.sh` runs any changed `*-test.sh`, and the pre-commit hook runs
# lint-changed. A hook is invoked with GIT_DIR, GIT_INDEX_FILE and friends
# EXPORTED, and `git -C "$tmp" init` honours an inherited GIT_DIR over its own
# -C argument: it re-initialised the live repository instead of the fixture.
# The run set core.bare on the real checkout (detaching the main working
# tree), committed "base"/"main adds b"/"main adds d"/"picked adds p" onto
# real branches, and left `stale`/`landed`/`live`/`picked` behind. Nothing in
# the output said so — the first sign was `git log -1` answering "picked adds
# p" in a repository with 500k lines of history.
#
# So: strip the redirect variables, then PROVE the fixture is the fixture
# before a single commit lands in it. A test that can write to the repository
# under test is not a test.
set -euo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_NAMESPACE \
      GIT_PREFIX GIT_CEILING_DIRECTORIES GIT_CONFIG GIT_CONFIG_COUNT

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/branch-status.sh"
pass=0
fail=0

ok() { echo "  ok   $1"; pass=$((pass + 1)); }
no() { echo "  FAIL $1"; echo "       $2"; fail=$((fail + 1)); }

# `pwd -P` so the canary below compares resolved paths: on macOS mktemp hands
# back /var/folders/... while git reports /private/var/folders/....
tmp="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$tmp"' EXIT

git -C "$tmp" init -q -b main

# THE CANARY. Ask git, not the environment, which repository it just made.
fixture_dir="$(git -C "$tmp" rev-parse --absolute-git-dir)"
case "$fixture_dir" in
    "$tmp"/*) ;;
    *)
        echo "branch-status-test: REFUSING TO RUN." >&2
        echo "  fixture requested: $tmp" >&2
        echo "  git actually used: $fixture_dir" >&2
        echo "Something outside this script is redirecting git — an exported" >&2
        echo "GIT_DIR from a hook is the usual cause. Continuing would commit" >&2
        echo "the fixture's history into that repository." >&2
        exit 2 ;;
esac

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

# A branch whose commits were CHERRY-PICKED into main. Its tip is not an
# ancestor, so ancestry calls it unlanded; every commit nonetheless has a
# patch-equivalent in main, which is how a rebase- or squash-merged branch
# looks. main then moves on, so this branch ALSO removes a file main has —
# the landed verdict must win over WOULD DELETE, and must not exit 3.
git -C "$tmp" checkout -q -b picked
echo p > "$tmp/p.txt"
git -C "$tmp" add p.txt
git -C "$tmp" commit -qm "picked adds p"
picked_sha="$(git -C "$tmp" rev-parse picked)"
git -C "$tmp" checkout -q main
# main moves first, so the pick lands on a different parent and cannot come
# out with the same object id — otherwise this fixture tests ancestry again.
echo d > "$tmp/d.txt"
git -C "$tmp" add d.txt
git -C "$tmp" commit -qm "main adds d"
git -C "$tmp" cherry-pick "$picked_sha" >/dev/null

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

# Patch-id: the ancestry test alone calls `picked` unlanded and destructive.
out="$(run picked)"
case "$out" in *"LANDED squashed/rebased"*) ok "a cherry-picked branch reads LANDED by patch id" ;;
    *) no "a cherry-picked branch reads LANDED by patch id" "$out" ;; esac
case "$out" in *"WOULD DELETE"*) no "the landed verdict wins over WOULD DELETE" "$out" ;;
    *) ok "the landed verdict wins over WOULD DELETE" ;; esac
if [ "$(rc picked)" = 0 ]; then ok "a patch-equivalent branch exits 0"; else no "a patch-equivalent branch exits 0" "got $(rc picked)"; fi

# ...and the escape hatch really does turn the patch-id check off, which is
# what proves the LANDED verdict above came from `git cherry` and not from
# ancestry: without it the same branch is destructive and exits 3.
out="$(run --no-patch-id picked)"
case "$out" in *"WOULD DELETE"*) ok "--no-patch-id falls back to the ancestry-only verdict" ;;
    *) no "--no-patch-id falls back to the ancestry-only verdict" "$out" ;; esac
if [ "$(rc --no-patch-id picked)" = 3 ]; then ok "--no-patch-id still exits 3 on a destructive branch"; else no "--no-patch-id still exits 3 on a destructive branch" "got $(rc --no-patch-id picked)"; fi

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
# The whole point of the patch-id verdict is that these drop out of the list
# too — they are the bulk of a squash-merging repository's branch pile.
if [[ "$out" =~ (^|$'\n')picked[[:space:]] ]]; then
    no "--stale-only hides patch-equivalent branches" "$out"
else
    ok "--stale-only hides patch-equivalent branches"
fi
# ...and it must still SHOW the branches that matter.
if [[ "$out" =~ (^|$'\n')stale[[:space:]] ]] && [[ "$out" =~ (^|$'\n')live[[:space:]] ]]; then
    ok "--stale-only still shows unlanded and destructive branches"
else
    no "--stale-only still shows unlanded and destructive branches" "$out"
fi

echo "branch-status-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
