#!/usr/bin/env bash
# GET /v1/sac-wrappers — the SAC (Stellar Asset Contract) wrapper
# registry: a map from Soroban contract address to the
# "<CODE>:<G-strkey>" form of the classic asset it wraps.
#
# Use it to resolve `transfer` events on SAC contracts back to the
# underlying classic asset.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"

curl -sS --fail "$BASE/v1/sac-wrappers"
echo
