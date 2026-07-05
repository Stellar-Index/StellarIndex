#!/usr/bin/env bash
# GET /v1/assets — top assets by 24h volume.
#
# Anonymous-friendly. Returns the asset listing rows powering
# stellarindex.io. Each row includes the coin-equivalence overlay
# (slug, code, issuer, last_price_usd, volume_24h_usd,
# market_cap_usd, sparkline_7d, etc.) inlined into the response —
# the standalone /v1/coins route was removed in rc.48 and the
# overlay was lifted onto every /v1/assets row in rc.47.
#
# Row filters (BACKLOG #54), combinable:
#   type=native|classic|soroban|fiat|any   structural asset class
#   code=USDC                              exact, case-sensitive code
#   issuer=G...                            issuing account G-strkey
# `code` + `issuer` together pin a single classic asset; malformed
# values return 400. (These narrow the default classic listing; the
# `asset_class` chip is the major class dispatch — see the spec.)
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
LIMIT="${1:-10}"

curl -sS --fail "$BASE/v1/assets?limit=$LIMIT&order=volume_24h_usd:desc"
echo

# Filtered examples (uncomment to run):
#   curl -sS --fail "$BASE/v1/assets?code=USDC"
#   curl -sS --fail "$BASE/v1/assets?issuer=GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&code=USDC"
