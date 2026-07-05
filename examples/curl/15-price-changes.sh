#!/usr/bin/env bash
# GET /v1/price/changes — current price plus the signed change over
# 1h / 24h / 7d / 30d in ONE call (wallet/portfolio delta strip).
#
# Each horizon carries `reference_at` + `resolution` so you see which
# closed bucket (and at what granularity) the delta was measured
# against. A horizon with no data that far back is `available: false`
# with null fields — never an error, so a fresh listing still returns
# its 1h/24h moves.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
ASSET="${1:-native}"
QUOTE="${2:-fiat:USD}"

curl -sS --fail "$BASE/v1/price/changes?asset=$ASSET&quote=$QUOTE"
echo
