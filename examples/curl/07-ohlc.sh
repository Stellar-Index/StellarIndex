#!/usr/bin/env bash
# GET /v1/ohlc — single open/high/low/close bar over [from, to).
#
# Defaults: from = to - 1h, to = now. Pass RFC 3339 timestamps to
# narrow. `truncated=true` in the response means the window had
# more trades than the per-request cap (10000) and the bar reflects
# only the chronologically-first N — narrow the window.
set -euo pipefail
# Defaults to USDC/XLM, the highest-volume on-chain pair.
# /v1/ohlc requires actual on-chain trade data — fiat quotes
# (`fiat:USD`) only work on /v1/price (synthesised from external CEX).
BASE="${API_BASE_URL:-https://api.stellaratlas.xyz}"
BASE_ASSET="${1:-USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN}"
QUOTE="${2:-native}"

if [ "${3:-}" ] && [ "${4:-}" ]; then
  curl -sS --fail "$BASE/v1/ohlc?base=$BASE_ASSET&quote=$QUOTE&from=$3&to=$4"
else
  curl -sS --fail "$BASE/v1/ohlc?base=$BASE_ASSET&quote=$QUOTE"
fi
echo
