#!/usr/bin/env bash
# GET /v1/assets — top assets by 24h volume.
#
# Anonymous-friendly. Returns the asset listing rows powering
# stellarindex.io. Each row includes the coin-equivalence overlay
# (slug, code, issuer, last_price_usd, volume_24h_usd,
# market_cap_usd, sparkline_7d, etc.) inlined into the response —
# there is no standalone /v1/coins route; the overlay is on every
# /v1/assets row.
#
# Row filters (BACKLOG #54), combinable:
#   type=native|classic|soroban|fiat|any   structural asset class
#   code=USDC                              exact, case-sensitive code
#   issuer=G...                            issuing account G-strkey
# `code` + `issuer` together pin a single classic asset; malformed
# values return 400. (These narrow the default classic listing; the
# `asset_class` chip is the major class dispatch — see the spec.)
#
# Ranking is `order_by`, NOT `order`: the handler accepts exactly
# `observation_count_desc` (the default) or `volume_24h_usd_desc`, and
# 400s on anything else. A bare `order=` is ignored, so asking for it
# silently returns the observation-count ranking.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
LIMIT="${1:-10}"

curl -sS --fail "$BASE/v1/assets?limit=$LIMIT&order_by=volume_24h_usd_desc"
echo

# Filtered examples (uncomment to run):
#   curl -sS --fail "$BASE/v1/assets?code=USDC"
#   curl -sS --fail "$BASE/v1/assets?issuer=GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&code=USDC"
