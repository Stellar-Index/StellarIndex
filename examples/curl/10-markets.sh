#!/usr/bin/env bash
# GET /v1/markets — distinct (base, quote) pairs traded in the last 14 days.
#
# Cursor-paginated. Pass the `next_cursor` from the previous
# response as the first arg to continue.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
LIMIT="${1:-50}"
CURSOR="${2:-}"

if [ -n "$CURSOR" ]; then
  curl -sS --fail "$BASE/v1/markets?limit=$LIMIT&cursor=$CURSOR"
else
  curl -sS --fail "$BASE/v1/markets?limit=$LIMIT"
fi
echo
