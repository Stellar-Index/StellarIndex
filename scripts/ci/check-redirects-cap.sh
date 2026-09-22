#!/usr/bin/env bash
# check-redirects-cap.sh — Cloudflare Pages free-plan _redirects rule cap
# (T335).
#
# web/explorer/public/_redirects itself documents CF Pages' free-plan
# ~100-rule limit (see its "fiat de-dup" and T247 comments) and asserts
# it stays under that cap by hand-counting whenever a rule is added.
# Nothing in CI checked it: `grep -rln _redirects .github/workflows/`
# only matched an unrelated web/status comment. Past the cap, CF Pages
# silently drops the excess rules — a deploy-time failure with no build
# error, discovered only by a redirect quietly not firing in production.
#
# Counts non-comment, non-blank lines (one _redirects rule per line) and
# fails if the count exceeds the cap.
#
# Usage: check-redirects-cap.sh [file] [cap]
#   file  default: web/explorer/public/_redirects
#   cap   default: 100 (Cloudflare Pages free-plan rule limit)
set -euo pipefail

FILE="${1:-web/explorer/public/_redirects}"
CAP="${2:-100}"

if [ ! -f "$FILE" ]; then
  echo "check-redirects-cap: $FILE not found" >&2
  exit 1
fi

count="$(grep -vcE '^[[:space:]]*(#|$)' "$FILE")"

if [ "$count" -gt "$CAP" ]; then
  echo "check-redirects-cap: $FILE has $count rules, exceeding Cloudflare Pages' free-plan cap of $CAP. Past this cap CF Pages silently drops the excess rules at deploy time — trim or consolidate rules (see the file's own de-dup comments) before adding more." >&2
  exit 1
fi

echo "check-redirects-cap: OK — $FILE has $count/$CAP rules."
