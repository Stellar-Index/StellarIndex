#!/usr/bin/env bash
#
# explorer-network-hosts.sh — point the static export's _redirects and
# llms.txt at the API host of the network being built. Both files live in
# public/ and name the mainnet API; shipped unchanged into a testnet or
# futurenet build they 301 that host's /v1/* to mainnet data and tell an
# agent reading llms.txt that mainnet is this network's API.
#
# Usage: scripts/ci/explorer-network-hosts.sh [out-dir]
# Reads NEXT_PUBLIC_API_BASE_URL (unset or the mainnet host: no rewrite).
set -uo pipefail

OUT="${1:-web/explorer/out}"
MAINNET="https://api.stellarindex.io"
API="${NEXT_PUBLIC_API_BASE_URL:-$MAINNET}"
API="${API%/}"

if [ ! -d "$OUT" ]; then
  echo "explorer-network-hosts: '$OUT' not found — build the explorer first." >&2
  exit 2
fi
if [ "$API" = "$MAINNET" ]; then
  echo "explorer-network-hosts: mainnet API host, nothing to rewrite."
  exit 0
fi
if ! [[ "$API" =~ ^https?://[A-Za-z0-9.:-]+$ ]]; then
  echo "explorer-network-hosts: NEXT_PUBLIC_API_BASE_URL '$API' is not a bare origin." >&2
  exit 1
fi

rc=0
for f in _redirects llms.txt; do
  if [ ! -f "$OUT/$f" ]; then
    echo "explorer-network-hosts: $OUT/$f is missing." >&2
    rc=1
    continue
  fi
  # `|` cannot occur in the origin (checked above), so it is a safe delimiter.
  sed "s|$MAINNET|$API|g" "$OUT/$f" >"$OUT/$f.tmp" && mv "$OUT/$f.tmp" "$OUT/$f"
  if grep -qF "$MAINNET" "$OUT/$f"; then
    echo "explorer-network-hosts: $OUT/$f still names $MAINNET." >&2
    rc=1
  fi
done
[ "$rc" -eq 0 ] && echo "explorer-network-hosts: _redirects and llms.txt point at $API."
exit "$rc"
