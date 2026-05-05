#!/usr/bin/env bash
# GET /v1/price — current VWAP price for one asset.
#
# `asset` is the canonical identifier from /v1/assets:
#   - native              — XLM
#   - USDC-GA5ZSEJYB37... — USDC
#   - <code>-<G-strkey>   — any classic asset
#   - C<contract-id>      — any Soroban SEP-41 token
#
# `quote` defaults to fiat:USD. Other valid forms: a canonical
# `<code>-<issuer>` identifier (cross-asset rate) or `fiat:EUR` /
# `fiat:GBP` / etc. (other fiat quotes).
set -euo pipefail
BASE="${API_BASE_URL:-https://api.ratesengine.net}"
ASSET="${1:-native}"
QUOTE="${2:-fiat:USD}"

curl -sS --fail "$BASE/v1/price?asset=$ASSET&quote=$QUOTE"
echo
