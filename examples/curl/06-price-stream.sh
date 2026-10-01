#!/usr/bin/env bash
# GET /v1/price/stream — Server-Sent Events tick stream for one asset.
#
# Streams every closed-bucket price update for the requested
# `(asset, quote)` pair as `data: {...}\n\n` SSE frames. Per ADR-0015,
# every subscriber on the same pair receives byte-identical payloads.
# Heartbeats every 15 s keep proxies happy. Ctrl-C to stop.
#
# SSE connections drop (deploys, proxies, slow-consumer eviction). On a
# drop this reconnects and sends the last received `id:` as
# Last-Event-ID, so the server replays what was missed instead of the
# client silently skipping it. curl --retry rides out a 5xx or 429 while
# reconnecting; any other HTTP error (bad pair, bad key) stops the loop.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
ASSET="${1:-native}"
QUOTE="${2:-fiat:USD}"

# The reader runs in a pipeline subshell, so it hands the id back via a file.
last_id_file=$(mktemp)
trap 'rm -f "$last_id_file"' EXIT

while :; do
  resume=()
  last_id=$(cat "$last_id_file")
  [ -n "$last_id" ] && resume=(-H "Last-Event-ID: $last_id")

  set +e
  curl -N -sS --fail --retry 5 --retry-connrefused \
    -H 'Accept: text/event-stream' \
    ${resume[@]+"${resume[@]}"} \
    "$BASE/v1/price/stream?asset=$ASSET&quote=$QUOTE" |
    while IFS= read -r line; do
      printf '%s\n' "$line"
      case "$line" in
        id:*) id="${line#id:}"; printf '%s' "${id# }" >"$last_id_file" ;;
      esac
    done
  rc=${PIPESTATUS[0]}
  set -e

  # 22 is curl --fail's HTTP-error exit, left after --retry gave up.
  [ "$rc" -eq 22 ] && exit "$rc"
  echo "stream ended (curl exit $rc); reconnecting in 2 s" >&2
  sleep 2
done
