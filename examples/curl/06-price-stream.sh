#!/usr/bin/env bash
# GET /v1/price/stream — Server-Sent Events tick stream for one asset.
#
# Streams every closed-bucket price update for the requested
# `(asset, quote)` pair as `data: {...}\n\n` SSE frames. Per ADR-0015,
# every subscriber on the same pair receives byte-identical payloads.
# Heartbeats every 15 s keep proxies happy. Ctrl-C to stop.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
ASSET="${1:-native}"
QUOTE="${2:-fiat:USD}"

curl -N -sS --fail \
  -H 'Accept: text/event-stream' \
  "$BASE/v1/price/stream?asset=$ASSET&quote=$QUOTE"
