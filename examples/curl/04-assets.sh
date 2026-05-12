#!/usr/bin/env bash
# GET /v1/assets — top assets by 24h volume.
#
# Anonymous-friendly. Returns the asset listing rows powering
# ratesengine.net. Each row includes the coin-equivalence overlay
# (slug, code, issuer, last_price_usd, volume_24h_usd,
# market_cap_usd, sparkline_7d, etc.) inlined into the response —
# the standalone /v1/coins route was removed in rc.48 and the
# overlay was lifted onto every /v1/assets row in rc.47.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.ratesengine.net}"
LIMIT="${1:-10}"

curl -sS --fail "$BASE/v1/assets?limit=$LIMIT&order=volume_24h_usd:desc"
echo
