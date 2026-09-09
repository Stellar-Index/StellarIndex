#!/usr/bin/env bash
# Answer, per branch, the only question that matters before you act on it:
# does it still carry work main does not have, and would applying it DESTROY
# anything?
#
# WHY THIS EXISTS (2026-09-08). Assessing a stale branch with
#
#     git diff --stat main...branch -- <paths>
#
# showed a tidy "5 files, +10/-18" and read as a small change still to land.
# Three dots is the MERGE-BASE diff: it answers "what did this branch do since
# it forked", INCLUDING everything that has since reached main by another
# route. The branch was 8 ahead / 104 behind and its tip was already an
# ancestor of main; the two-dot diff showed it would have reverted files to
# older text and DELETED 158 files present on main — among them source
# written that same day, and the restore-drill and ClickHouse-exporter
# infrastructure added that week.
# An agent was briefed to cherry-pick it on the three-dot reading. It
# verified the premise instead of executing it, which is the only reason
# nothing was lost.
#
# So the rule this script encodes: never judge a branch by `main...branch`.
# Judge it by whether its tip is an ancestor of main, by `main..branch`
# (two-dot) for commits, and by the DELETE lines in `git diff --name-status`.
#
# ANCESTRY IS NOT THE ONLY WAY WORK LANDS (2026-09-09). Over the 143 local
# branches this repository had that day, the ancestor test called 10 of them
# LANDED. A per-branch content audit put the real figure at 106. The gap is
# how this repository merges: a rebase and a squash PR both re-author the
# commit, so the tip is not an ancestor of the base and every file reads as
# changed — a landed branch is the same shape as a forgotten one. Reporting
# 96 landed branches as "unlanded, would delete 500 files" is how a report
# gets ignored, which is the original failure wearing different clothes.
#
# `git cherry` closes part of it: it matches by PATCH ID, which survives a
# rebase or a cherry-pick, so a branch whose every commit reads "-" is in.
# Note the deletion consequence — `git branch -d` checks ancestry too, so it
# REFUSES these and the delete needs `-D`. The verdict says so.
#
# What patch id cannot see is a squash of SEVERAL commits: one squashed
# commit has one patch id and it equals none of the originals. Those branches
# still read as unlanded here, correctly — the script will not guess. Settle
# them by hand: find the squash (`git log --oneline BASE --grep=<issue>`),
# then confirm the branch's added lines are in the base's tree before
# deleting. docs/operations/branch-triage.md records that procedure and the
# 2026-09-09 pass that applied it.
#
# Usage:
#   scripts/dev/branch-status.sh                 every local branch
#   scripts/dev/branch-status.sh <branch>...     just these
#   scripts/dev/branch-status.sh --stale-only    hide branches that are LANDED
#   scripts/dev/branch-status.sh --no-patch-id   ancestry only; skip git cherry
#   BASE=origin/main scripts/dev/branch-status.sh
#
# `git cherry` costs one patch id per commit the branch is BEHIND. Measured
# 2026-09-09: 38s for 143 branches averaging 400 behind, against 9s without.
# --no-patch-id buys that back at the cost of the rebase verdict.
#
# Exit status is a REPORT, not a judgement: 0 always, unless a branch would
# delete files that exist on the base (exit 3) — that is the case worth
# refusing to automate around.
set -euo pipefail

BASE="${BASE:-main}"
STALE_ONLY=false
PATCH_ID=true
BRANCHES=()

for arg in "$@"; do
    case "$arg" in
        --stale-only) STALE_ONLY=true ;;
        --no-patch-id) PATCH_ID=false ;;
        -h|--help) sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        -*) echo "branch-status: unknown flag: $arg" >&2; exit 2 ;;
        *) BRANCHES+=("$arg") ;;
    esac
done

git rev-parse --verify --quiet "$BASE" >/dev/null || {
    echo "branch-status: base '$BASE' does not resolve" >&2; exit 2; }

if [ ${#BRANCHES[@]} -eq 0 ]; then
    # Every local branch except the base itself. Worktree scratch branches
    # (bootstrap-worktree.sh names them worktree-*) are noise here.
    while IFS= read -r b; do
        [ "$b" = "$BASE" ] && continue
        case "$b" in worktree-*) continue ;; esac
        BRANCHES+=("$b")
    done < <(git for-each-ref --format='%(refname:short)' refs/heads/)
fi

danger=0
printf '%-52s %6s %7s  %s\n' "BRANCH" "AHEAD" "BEHIND" "VERDICT"
printf '%-52s %6s %7s  %s\n' "------" "-----" "------" "-------"

for b in "${BRANCHES[@]}"; do
    if ! git rev-parse --verify --quiet "$b" >/dev/null; then
        printf '%-52s %6s %7s  %s\n' "$b" "-" "-" "UNKNOWN REF"
        continue
    fi

    # Two-dot everywhere. `main..branch` is commits the branch has that the
    # base does not; `branch..main` is the reverse. Never three dots.
    ahead=$(git rev-list --count "$BASE".."$b")
    behind=$(git rev-list --count "$b".."$BASE")

    # The decisive question, and it is one command: is the tip already in?
    landed=false
    if git merge-base --is-ancestor "$b" "$BASE" 2>/dev/null; then
        landed=true
    fi

    # And the question ancestry cannot answer: did the work land WITHOUT the
    # commits? `git cherry` prints "+" for a commit with no patch-equivalent
    # in the base and "-" for one that has an equivalent, so no "+" at all
    # means a squash or a rebase carried every commit in.
    squashed=false
    if [ "$landed" = false ] && [ "$PATCH_ID" = true ] && [ "$ahead" -gt 0 ]; then
        unmatched=$(git cherry "$BASE" "$b" 2>/dev/null | grep -c '^+' || true)
        [ "$unmatched" -eq 0 ] && squashed=true
    fi

    # What would applying this branch DELETE that the base still has? This is
    # the line the three-dot diff cannot show you, because a file added to
    # main after the fork simply is not in the merge-base comparison.
    deletes=$(git diff --name-status "$BASE" "$b" 2>/dev/null | awk '$1 ~ /^D/ {print $2}')
    ndel=$(printf '%s' "$deletes" | grep -c . || true)

    if [ "$landed" = true ]; then
        verdict="LANDED — tip is an ancestor of $BASE; safe to delete (-d)"
        $STALE_ONLY && continue
    elif [ "$squashed" = true ]; then
        verdict="LANDED squashed/rebased — all $ahead commit(s) have a patch-equivalent in $BASE; delete with -D (-d refuses)"
        $STALE_ONLY && continue
    elif [ "$ndel" -gt 0 ]; then
        verdict="WOULD DELETE $ndel file(s) present on $BASE — do NOT apply wholesale"
        danger=1
    elif [ "$ahead" -eq 0 ]; then
        verdict="no unlanded commits"
        $STALE_ONLY && continue
    else
        verdict="$ahead unlanded commit(s)"
    fi

    printf '%-52s %6s %7s  %s\n' "$b" "$ahead" "$behind" "$verdict"

    if [ "$ndel" -gt 0 ]; then
        # No pipe into head: under pipefail an early-exit consumer turns a
        # successful producer into a failure. Bound it in the shell instead.
        shown=0
        while IFS= read -r d; do
            [ -z "$d" ] && continue
            echo "      would delete: $d"
            shown=$((shown + 1))
            [ "$shown" -ge 8 ] && break
        done <<EOF
$deletes
EOF
        if [ "$ndel" -gt 8 ]; then
            echo "      … and $((ndel - 8)) more"
        fi
    fi
done

if [ "$danger" -eq 1 ]; then
    cat >&2 <<'EOF'

At least one branch would DELETE files that exist on the base. That is what a
long-stale branch does: it does not carry the work done since it forked, so
applying it wholesale removes that work. Rebase it, or cherry-pick the
specific commits you want after checking each one's --name-status.
EOF
    exit 3
fi
