#!/usr/bin/env bash
# check-dependabot-toolchain-bump.sh — refuse a Dependabot PR that rides a
# language-toolchain change in under a dependency-patch label.
#
# The incident this pins (#495):
# the go-minor-patch Dependabot group raised `go 1.25.10` + `toolchain
# go1.25.13` to `go 1.26.0` (toolchain directive dropped) alongside eight
# ordinary module bumps in one grouped PR. Dependabot's semver parser
# reads 1.25 -> 1.26 as a "minor" bump because it treats the `go`
# directive like any other module version, but for the Go toolchain a
# minor version IS a language/runtime change — new vet rules, a new
# `go` binary requirement, and (the concrete failure here) a
# `govulncheck` built against go1.25 that cannot analyse a go1.26
# module graph at all, so the vulnerability gate went dark with a
# message that looked like an unrelated tool failure, not a reviewable
# decision. A human bumping the Go version chooses to and says so in
# the PR; Dependabot's grouped-patch label hides that choice.
#
# THE RULE. Only PRs authored by dependabot[bot] are in scope — a human
# raising the Go version is a deliberate, reviewed decision and this
# gate has nothing to add to it. Within a dependabot PR, a diff that
# touches the `go` or `toolchain` directive line of go.mod, or the
# `engines` / `packageManager` key of any package.json, fails closed
# and names the exact line, so the PR is rejected with the right reason
# instead of merged and diagnosed later from a govulncheck error.
#
# Usage:
#   git diff "$BASE_SHA" "$HEAD_SHA" -- go.mod '**/package.json' \
#     | check-dependabot-toolchain-bump.sh "$PR_AUTHOR"
#
# Reads a unified diff (as produced by `git diff`) on stdin. Only the
# `+++ b/<path>` file headers and `+`/`-` content lines are used, so a
# diff produced by `diff -u old new` (as the fixtures below do, with no
# git repository at all) works identically.
#
# Exit 0 — PR author is not dependabot[bot], or the diff has no
#          toolchain-version line. Nothing to block.
# Exit 1 — a dependabot[bot] PR touches a toolchain-version line. Blocked.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: check-dependabot-toolchain-bump.sh <pr-author-login>" >&2
  exit 2
fi
PR_AUTHOR="$1"

# Always drain stdin first, even when the actor check below will make the
# result moot — a `git diff ... | check-dependabot-toolchain-bump.sh`
# caller must never see a broken pipe because this script decided the
# input didn't matter and exited early (lint-shell-sigpipe.sh's class of
# defect, applied to a consumer rather than a `head`/`sort` producer).
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
