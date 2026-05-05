#!/usr/bin/env bash
# GET /v1/coins — top assets by 24h volume.
#
# Anonymous-friendly. Returns the same shape powering ratesengine.net
# (slug, code, issuer, last_price, volume_24h, change_24h).
set -euo pipefail
BASE="${API_BASE_URL:-https://api.ratesengine.net}"
LIMIT="${1:-10}"

curl -sS --fail "$BASE/v1/coins?limit=$LIMIT"
echo
