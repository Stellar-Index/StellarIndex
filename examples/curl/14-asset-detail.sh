#!/usr/bin/env bash
# GET /v1/assets/{id} — full detail for one asset: identity,
# SEP-1 metadata status, supply, 24h volume, price changes.
#
# `id` is the canonical identifier from /v1/assets:
#   - native              — XLM
#   - <code>-<G-strkey>   — any classic asset
#   - C<contract-id>      — any Soroban SEP-41 token
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
ASSET="${1:-USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN}"

curl -sS --fail "$BASE/v1/assets/$ASSET"
echo
