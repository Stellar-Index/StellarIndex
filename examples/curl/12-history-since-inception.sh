#!/usr/bin/env bash
# GET /v1/history/since-inception — the full bucketed VWAP series
# for an asset, from its first observed trade to now.
#
# `granularity` is one of 1m / 15m / 1h / 4h / 1d / 1w / 1mo
# (default 1d). Fine granularities over long-lived assets return
# large payloads — the daily default keeps this snappy.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
ASSET="${1:-native}"
QUOTE="${2:-fiat:USD}"
GRANULARITY="${3:-1d}"

curl -sS --fail "$BASE/v1/history/since-inception?asset=$ASSET&quote=$QUOTE&granularity=$GRANULARITY"
echo
