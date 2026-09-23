#!/usr/bin/env bash
# check-no-wrangler-cache-tracked.sh — tripwire for T535: a Cloudflare
# Pages `.wrangler/` build-cache directory (contains `account_id`,
# `project_name`) was committed at docs/reference/api/.wrangler/cache/
# pages.json in a public repo. .gitignore's `/.wrangler/` entry is
# root-anchored and never matched that nested path.
#
# Asserts two things against the CURRENT tree (default) or a given repo
# root (arg 1, for the test fixture):
#   1. no `.wrangler/` path is tracked by git anywhere in the tree.
#   2. a synthetic nested `.wrangler/` path IS matched by .gitignore,
#      i.e. the ignore pattern is not root-anchored.
#
# Exit code: 0 = clean, 1 = a `.wrangler/` path is tracked or the
# nested-path ignore check fails.
set -uo pipefail

REPO_ROOT="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
cd "$REPO_ROOT" || exit 1

problems=0

tracked="$(git ls-files | grep -F '.wrangler/' || true)"
if [ -n "$tracked" ]; then
  echo "FAIL: .wrangler/ path(s) tracked by git:" >&2
  echo "$tracked" >&2
  problems=$((problems + 1))
fi

probe="docs/reference/api/.wrangler/cache/pages.json"
if ! git check-ignore -q "$probe"; then
  echo "FAIL: nested path '$probe' is not gitignored (pattern is root-anchored)" >&2
  problems=$((problems + 1))
fi

if [ "$problems" -gt 0 ]; then
  echo "check-no-wrangler-cache-tracked: FAIL ($problems problem(s))"
  exit 1
fi

echo "check-no-wrangler-cache-tracked: OK"
exit 0
