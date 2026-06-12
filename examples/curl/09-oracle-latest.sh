#!/usr/bin/env bash
# GET /v1/oracle/latest — last oracle reading per source for an asset.
#
# Returns one entry per source that has observed `asset`:
# reflector-dex / reflector-cex / reflector-fx / redstone / band.
# Optional `source` filter narrows to a single oracle.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellaratlas.xyz}"
ASSET="${1:-native}"

if [ "${2:-}" ]; then
  curl -sS --fail "$BASE/v1/oracle/latest?asset=$ASSET&source=$2"
else
  curl -sS --fail "$BASE/v1/oracle/latest?asset=$ASSET"
fi
echo
