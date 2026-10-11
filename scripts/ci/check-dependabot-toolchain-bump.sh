#!/usr/bin/env bash
# check-dependabot-toolchain-bump.sh — refuse a Dependabot PR that rides a
# language-toolchain change in under a dependency-patch label.
#
# Dependabot's semver parser reads a `go` directive bump 1.25 -> 1.26 as a
# "minor" module bump, so the go-minor-patch group can carry it. For the Go
# toolchain a minor IS a language/runtime change: new vet rules, a new `go`
# binary requirement, and a govulncheck built on the old toolchain that cannot
# analyse the new module graph (the vulnerability gate goes dark with an
# unrelated-looking error). A human bumping Go says so in the PR; the grouped
# label hides that choice.
#
# THE RULE. Only PRs authored by dependabot[bot] are in scope. In one, a diff
# touching the `go` or `toolchain` line of go.mod, or the `engines` /
# `packageManager` key of any package.json, fails closed and names the line.
#
# Usage:
#   git diff "$BASE_SHA" "$HEAD_SHA" -- go.mod '**/package.json' \
#     | check-dependabot-toolchain-bump.sh "$PR_AUTHOR"
#
# Reads a unified diff on stdin; only `+++ b/<path>` headers and `+`/`-`
# content lines are used, so `diff -u old new` fixtures (no git repo) work.
#
# Exit 0 — author is not dependabot[bot], or no toolchain-version line changed.
# Exit 1 — a dependabot[bot] PR touches a toolchain-version line. Blocked.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: check-dependabot-toolchain-bump.sh <pr-author-login>" >&2
  exit 2
fi
PR_AUTHOR="$1"

# Always drain stdin first, even when the author check makes it moot: an
# early exit would hand a `git diff | …` caller a broken pipe
# (lint-shell-sigpipe.sh's class of defect, on the consumer side).
DIFF_TEXT="$(cat)"

if [ "$PR_AUTHOR" != "dependabot[bot]" ]; then
  exit 0
fi

VIOLATIONS="$(printf '%s\n' "$DIFF_TEXT" | awk '
  /^\+\+\+ / {
    file = $2
    sub(/^b\//, "", file)
    next
  }
  file ~ /(^|\/)go\.mod$/ && /^[+-][[:space:]]*(go|toolchain)[[:space:]]/ {
    print file ": " $0
  }
  file ~ /(^|\/)package\.json$/ && /^[+-].*"(engines|packageManager)"[[:space:]]*:/ {
    print file ": " $0
  }
')"

if [ -z "$VIOLATIONS" ]; then
  exit 0
fi

echo "::error::check-dependabot-toolchain-bump: a Dependabot PR changes a language-toolchain directive. A go/toolchain version bump (or an engines/packageManager pin) is a deliberate decision, never a dependency patch — split it out of the grouped PR and land it as a reviewed, standalone change (and re-pin govulncheck's build toolchain in the same change; see ci.yml's 'govulncheck toolchain parity' step)." >&2
printf '%s\n' "$VIOLATIONS" | sed 's/^/    /' >&2
exit 1
