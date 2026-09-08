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
# Usage:
#   scripts/dev/branch-status.sh                 every local branch
#   scripts/dev/branch-status.sh <branch>...     just these
#   scripts/dev/branch-status.sh --stale-only    hide branches that are LANDED
#   BASE=origin/main scripts/dev/branch-status.sh
#
# Exit status is a REPORT, not a judgement: 0 always, unless a branch would
# delete files that exist on the base (exit 3) — that is the case worth
# refusing to automate around.
set -euo pipefail

BASE="${BASE:-main}"
STALE_ONLY=false
BRANCHES=()

for arg in "$@"; do
    case "$arg" in
        --stale-only) STALE_ONLY=true ;;
        -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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

    # What would applying this branch DELETE that the base still has? This is
    # the line the three-dot diff cannot show you, because a file added to
    # main after the fork simply is not in the merge-base comparison.
    deletes=$(git diff --name-status "$BASE" "$b" 2>/dev/null | awk '$1 ~ /^D/ {print $2}')
    ndel=$(printf '%s' "$deletes" | grep -c . || true)

    if [ "$landed" = true ]; then
        verdict="LANDED — tip is an ancestor of $BASE; safe to delete"
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
