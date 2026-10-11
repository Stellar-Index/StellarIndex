#!/usr/bin/env bash
# commit-identity-range.sh — computes the `git log` range that
# .github/workflows/commit-identity.yml's two jobs (identity,
# self-attribution) check. Kept out of the workflow so a test can pin it.
#
# New-branch/tag fallback: `--all` cannot be used, for two reasons:
#  1. actions/checkout leaves a detached HEAD, so `git rev-parse --abbrev-ref
#     HEAD` returns the literal "HEAD" and excluding it is a no-op.
#  2. `--all` also adds detached HEAD itself as a tip. HEAD is not under
#     refs/, so no `--exclude=<glob>` removes it, and `$SHA --not --all` is
#     always empty when SHA is what is checked out.
# So use `--branches --remotes --tags` (none add HEAD) and exclude the pushed
# ref by name from GITHUB_REF_NAME (set for every Actions event;
# `rev-parse --abbrev-ref HEAD` only as a local fallback). `--exclude` applies
# only to the NEXT collector, so the pair is repeated before each of the three.
#
# Env: EVENT, BEFORE, SHA, BASE_SHA, BASE_REF, HEAD_SHA, DEFAULT_BRANCH,
# GITHUB_REF_NAME (as set by the workflow's `env:` block). Prints the range as
# space-separated `git log` arguments — consume it unquoted: `git log $range`.
set -euo pipefail
cd "${RANGE_REPO:-$(dirname "$0")/../..}" || exit 1

EVENT="${EVENT:-}"
BEFORE="${BEFORE:-}"
SHA="${SHA:-}"
BASE_SHA="${BASE_SHA:-}"
BASE_REF="${BASE_REF:-}"
HEAD_SHA="${HEAD_SHA:-}"
DEFAULT_BRANCH="${DEFAULT_BRANCH:-}"

if [ "$EVENT" = pull_request ]; then
  # base.sha goes stale once the base branch moves; also exclude its tip.
  excl="$BASE_SHA"
  if [ -n "$BASE_REF" ] && git rev-parse --verify -q "origin/${BASE_REF}^{commit}" >/dev/null; then
    excl="$BASE_SHA origin/${BASE_REF}"
  fi
  echo "${HEAD_SHA} --not ${excl}"
  exit 0
fi

if [ -n "$BEFORE" ] && [ "$BEFORE" != "0000000000000000000000000000000000000000" ] \
  && git cat-file -e "${BEFORE}^{commit}" 2>/dev/null; then
  # A feature branch that merged the default branch carries commits already
  # on it (e.g. a bot commit); judge only what the branch adds. Pushes to the
  # default branch itself stay on the full before..after range.
  if [ -n "$DEFAULT_BRANCH" ] && [ "${GITHUB_REF_NAME:-}" != "$DEFAULT_BRANCH" ] \
    && git rev-parse --verify -q "origin/${DEFAULT_BRANCH}^{commit}" >/dev/null; then
    echo "${BEFORE}..${SHA} --not origin/${DEFAULT_BRANCH}"
    exit 0
  fi
  echo "${BEFORE}..${SHA}"
  exit 0
fi

# A new branch, or a force-push whose predecessor is gone. Judge SHA on what
# it adds over every OTHER ref, excluding refs naming THIS push's branch/tag
# so a fetched copy cannot cancel SHA out.
ref_name="${GITHUB_REF_NAME:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo _)}"
echo "$SHA" --not \
  --exclude="$ref_name" --exclude="*/$ref_name" --branches \
  --exclude="$ref_name" --exclude="*/$ref_name" --remotes \
  --exclude="$ref_name" --exclude="*/$ref_name" --tags
