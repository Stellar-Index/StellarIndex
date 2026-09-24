#!/usr/bin/env bash
# commit-identity-range.sh — computes the `git log` range that
# .github/workflows/commit-identity.yml's two jobs (identity,
# self-attribution) check. Pulled out of the workflow so the range logic
# can be pinned by a test instead of only living inline in YAML.
#
# RLT-374: the new-branch/tag fallback used
# `git rev-parse --abbrev-ref HEAD` to name the ref to exclude from
# `--all`. Two independent bugs made it reachable-empty on the CI checkout
# this fallback exists for:
#
#  1. actions/checkout always leaves the tree in detached HEAD (even for a
#     plain push), so that command returns the literal string "HEAD" —
#     excluding a ref that does not exist is a no-op.
#  2. `--all` also always adds detached HEAD itself as an extra tip
#     (documented: "all the refs in refs/, along with HEAD"). HEAD is not
#     a name under refs/, so no `--exclude=<glob>` pattern can ever remove
#     it — `$SHA --not --all` is unconditionally empty whenever SHA is
#     what's currently checked out, i.e. always, for this job.
#
# Fix: never use `--all` here — use `--branches --remotes --tags`, none of
# which implicitly add HEAD, so a real exclude pattern can suppress every
# copy of the pushed ref. Name that ref from GITHUB_REF_NAME (set by the
# Actions runner for every event; `git rev-parse --abbrev-ref HEAD` only
# as a local/manual-run fallback). `--exclude` patterns apply only to the
# NEXT `--branches`/`--remotes`/`--tags`/`--all`/`--glob` and are then
# forgotten, so the pair is repeated before each of the three collectors
# rather than given once up front.
#
# Env: EVENT, BEFORE, SHA, BASE_SHA, HEAD_SHA, GITHUB_REF_NAME (all as set
# by the workflow's `env:` block). Prints the range as space-separated
# `git log` arguments — consume it unquoted, e.g. `git log $range`.
set -euo pipefail
cd "$(dirname "$0")/../.." || exit 1

EVENT="${EVENT:-}"
BEFORE="${BEFORE:-}"
SHA="${SHA:-}"
BASE_SHA="${BASE_SHA:-}"
HEAD_SHA="${HEAD_SHA:-}"

if [ "$EVENT" = pull_request ]; then
  echo "${BASE_SHA}..${HEAD_SHA}"
  exit 0
fi

if [ -n "$BEFORE" ] && [ "$BEFORE" != "0000000000000000000000000000000000000000" ] \
  && git cat-file -e "${BEFORE}^{commit}" 2>/dev/null; then
  echo "${BEFORE}..${SHA}"
  exit 0
fi

# A new branch, or a force-push whose predecessor is gone. Judge SHA on
# what it adds over every OTHER ref, excluding every ref that names THIS
# push's own branch/tag so a fetched copy of it cannot cancel SHA out of
# --all.
ref_name="${GITHUB_REF_NAME:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo _)}"
echo "$SHA" --not \
  --exclude="$ref_name" --exclude="*/$ref_name" --branches \
  --exclude="$ref_name" --exclude="*/$ref_name" --remotes \
  --exclude="$ref_name" --exclude="*/$ref_name" --tags
