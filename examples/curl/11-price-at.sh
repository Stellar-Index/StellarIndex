#!/usr/bin/env bash
# GET /v1/price/at — point-in-time price: the closed 1-minute VWAP
# bucket at-or-before `ts` (cost-basis / PnL / tax lookups).
#
# The response's `observed_at` is the BUCKET's close time — read it
# to see how far away the nearest observation was. A nearest bucket
# more than 24h before `ts` is a 404, not a fabricated price.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
ASSET="${1:-native}"
QUOTE="${2:-fiat:USD}"
# Default `ts`: 24 hours ago, RFC 3339 UTC (GNU + BSD date).
TS="${3:-$(date -u -d '24 hours ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
        || date -u -v-24H +%Y-%m-%dT%H:%M:%SZ)}"

curl -sS --fail "$BASE/v1/price/at?asset=$ASSET&quote=$QUOTE&ts=$TS"
echo
