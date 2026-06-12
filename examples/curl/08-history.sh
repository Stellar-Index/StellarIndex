#!/usr/bin/env bash
# GET /v1/history — per-trade records for a pair in [from, to).
#
# Defaults to the last 1 hour. Pagination via opaque `cursor` —
# the response includes the next cursor when more rows exist.
# Bucketed VWAP/TWAP series ship via /v1/vwap and /v1/twap.
set -euo pipefail
# Defaults to USDC/XLM, the highest-volume on-chain pair.
BASE="${API_BASE_URL:-https://api.stellaratlas.xyz}"
BASE_ASSET="${1:-USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN}"
QUOTE="${2:-native}"
LIMIT="${3:-100}"

curl -sS --fail "$BASE/v1/history?base=$BASE_ASSET&quote=$QUOTE&limit=$LIMIT"
echo
