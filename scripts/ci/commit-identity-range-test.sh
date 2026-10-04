#!/usr/bin/env bash
# commit-identity-range-test.sh — pins commit-identity-range.sh (RLT-374)
# against the exact scenario that broke it: a brand-new branch pushed
# under `fetch-depth: 0`, checked out detached (as actions/checkout always
# does), with the branch's own commit ALSO present as a remote-tracking
# ref (as a full-history fetch leaves it). The range must still contain
# the branch's new commit — the bug made it come back empty.
#
# Run: bash scripts/ci/commit-identity-range-test.sh
set -uo pipefail

# `git init` honours an INHERITED GIT_DIR ahead of its own `-C`, and a git
# hook exports GIT_DIR/GIT_INDEX_FILE. lint-changed dispatches test
# scripts, and the pre-commit hook runs lint-changed — so without this a
# fixture's init re-initialises the REAL repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/commit-identity-range.sh"

TMP="$(mktemp -d)"
export RANGE_REPO="$TMP/repo"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# mkrepo — a repo with one commit on main, then a brand-new branch
# "feature-x" with one more commit, checked out DETACHED at that commit
# (mimicking actions/checkout), and a refs/remotes/origin/feature-x ref
# pointing at the same commit (mimicking a fetch-depth:0 checkout of a
# push event, which fetches every branch as a remote-tracking ref).
mkrepo() {
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo"
  (
    cd "$TMP/repo" || exit 1
    git init -q .
    git config user.email t@t.invalid
    git config user.name t
    git config commit.gpgsign false
    printf 'one\n' > file.txt
    git add -A
    git commit -qm "base"
    git branch -m main
    git checkout -qb feature-x
    printf 'two\n' > file.txt
    git add -A
    git commit -qm "new work"
    NEW_SHA="$(git rev-parse HEAD)"
    echo "$NEW_SHA" > "$TMP/new_sha"
    git update-ref refs/remotes/origin/main "$(git rev-parse main)"
    git update-ref refs/remotes/origin/feature-x "$NEW_SHA"
    git checkout -q "$NEW_SHA"
  )
}

# range_for <ref_name> — runs the gate for a new-branch push of
# feature-x's tip, with GITHUB_REF_NAME set as the Actions runner sets it,
# and prints the resulting `git log` output (one line per commit reached).
range_for() {
  local ref_name="$1" new_sha range
  new_sha="$(cat "$TMP/new_sha")"
  (
    cd "$TMP/repo" || exit 1
    range="$(EVENT=push BEFORE=0000000000000000000000000000000000000000 \
      SHA="$new_sha" GITHUB_REF_NAME="$ref_name" "$GATE")"
    # shellcheck disable=SC2086
    git log $range --format='%H'
  )
}

check() { # <name> <expect: nonempty|empty>
  local name="$1" expect="$2" out new_sha
  new_sha="$(cat "$TMP/new_sha")"
  out="$(range_for feature-x)"
  if [ "$expect" = nonempty ]; then
    if [ -n "$out" ] && grep -qF "$new_sha" <<<"$out"; then
      printf '  ok   %s\n' "$name"
      pass=$((pass + 1))
    else
      printf '  FAIL %s (range came back empty or missing %s)\n' "$name" "$new_sha"
      fail=$((fail + 1))
    fi
  else
    if [ -z "$out" ]; then
      printf '  ok   %s\n' "$name"
      pass=$((pass + 1))
    else
      printf '  FAIL %s (expected empty, got: %s)\n' "$name" "$out"
      fail=$((fail + 1))
    fi
  fi
}

mkrepo
check "new branch under fetch-depth:0, detached HEAD: the branch's own commit is still checked" nonempty

# PR whose base.sha is older than the base branch tip: main gained a commit
# after the PR branched and the PR merged it in; that commit is not the PR's own.
mkrepo_pr() {
  mkrepo
  (
    cd "$TMP/repo" || exit 1
    git rev-parse main > "$TMP/stale_base"
    git checkout -q main
    printf 'moved\n' > other.txt
    git add -A
    git commit -qm "landed on main after the PR branched"
    git rev-parse HEAD > "$TMP/main_tip"
    git update-ref refs/remotes/origin/main "$(cat "$TMP/main_tip")"
    git checkout -q feature-x
    git merge -q --no-ff -m "merge main into PR" main
    git rev-parse HEAD > "$TMP/pr_head"
    git checkout -q "$(cat "$TMP/pr_head")"
  )
}

mkrepo_pr
# shellcheck disable=SC2086
pr_out="$(cd "$TMP/repo" && range="$(EVENT=pull_request BASE_SHA="$(cat "$TMP/stale_base")" \
  BASE_REF=main HEAD_SHA="$(cat "$TMP/pr_head")" "$GATE")" && git log $range --format='%H')"
if grep -qF "$(cat "$TMP/new_sha")" <<<"$pr_out" && ! grep -qF "$(cat "$TMP/main_tip")" <<<"$pr_out"; then
  printf '  ok   %s\n' "PR with stale base.sha: base-branch commit excluded, PR commit included"
  pass=$((pass + 1))
else
  printf '  FAIL PR with stale base.sha (got: %s)\n' "$pr_out"
  fail=$((fail + 1))
fi

# Push to a feature branch that merged main: the main-side commit (before
# the push, absent from BEFORE) must not be checked; the branch's own is.
mkrepo_pr
# shellcheck disable=SC2086
push_out="$(cd "$TMP/repo" && range="$(EVENT=push BEFORE="$(cat "$TMP/new_sha")" SHA="$(cat "$TMP/pr_head")" \
  DEFAULT_BRANCH=main GITHUB_REF_NAME=feature-x "$GATE")" && git log $range --format='%H')"
if [ -n "$push_out" ] && ! grep -qF "$(cat "$TMP/main_tip")" <<<"$push_out"; then
  printf '  ok   %s\n' "feature-branch push after merging main: main-side commit excluded"
  pass=$((pass + 1))
else
  printf '  FAIL feature-branch push after merging main (got: %s)\n' "$push_out"
  fail=$((fail + 1))
fi

# Push to main itself keeps the full before..after range.
# shellcheck disable=SC2086
main_out="$(cd "$TMP/repo" && range="$(EVENT=push BEFORE="$(cat "$TMP/stale_base")" SHA="$(cat "$TMP/main_tip")" \
  DEFAULT_BRANCH=main GITHUB_REF_NAME=main "$GATE")" && git log $range --format='%H')"
if grep -qF "$(cat "$TMP/main_tip")" <<<"$main_out"; then
  printf '  ok   %s\n' "push to default branch: full range kept"
  pass=$((pass + 1))
else
  printf '  FAIL push to default branch (got: %s)\n' "$main_out"
  fail=$((fail + 1))
fi

echo
echo "commit-identity-range-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
