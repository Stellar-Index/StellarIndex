#!/usr/bin/env bash
# shellcheck disable=SC2016 # $vars inside single quotes are jq variables, not shell ones.
# Capture real Reflector UpdateEvent fixtures from a live stellar-rpc.
#
# Writes one JSON fixture per matched event to
# test/fixtures/reflector/<wasm_hash>/<ledger>_<tx>.json. Consumed by
# internal/sources/reflector/decode_test.go (TODO: integration-tag replay);
# SDK-encoded tests cover the wire format today, these become the regression
# corpus once landed. Fixtures are pinned per WASM hash so old ledger ranges
# keep decoding after the contract upgrades
# (docs/architecture/ingest-pipeline.md#contract-schema-evolution).
#
# Usage: scripts/dev/capture-reflector-fixtures.sh [-e URL] [-c CONTRACT] [-n N] [-s LEDGER]
#   -e  stellar-rpc endpoint (default http://127.0.0.1:8000; on r1 the local
#       admin port, remote probing needs a tunnel or VPN).
#   -c  Reflector contract ID (default: DEX variant from VERSIONS.md; others:
#       CAFJZQWS... CEX, CBKGPWGK... FX).
#   -n  Max events (default 10). One update per ~5 min per variant, so 10
#       covers ~50 min.
#   -s  Start ledger (default latestLedger - 200); lower it for a longer window.
#
# Environment: JQ, CURL (binary paths, default jq / curl, both required);
# WASM_HASH (directory the fixtures land under, see below).
#
# Output per fixture:
#   {
#     "contract_id":  "C…",
#     "wasm_hash":    "…",                 -- from getLedgerEntries
#     "ledger":       52003412,
#     "tx_hash":      "deadbeef…",
#     "ledger_closed_at": "2026-04-23T…Z",
#     "topics":       ["AAAADwAA…","AAAADwAA…","AAAABQAA…"],
#     "value":        "AAAAEAAA…"
#   }

set -euo pipefail

ENDPOINT="${ENDPOINT:-http://127.0.0.1:8000}"
CONTRACT_ID="${CONTRACT_ID:-CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M}"
MAX_EVENTS="${MAX_EVENTS:-10}"
START_LEDGER=""
JQ="${JQ:-jq}"
CURL="${CURL:-curl}"

while getopts "e:c:n:s:h" opt; do
  case "$opt" in
    e) ENDPOINT="$OPTARG" ;;
    c) CONTRACT_ID="$OPTARG" ;;
    n) MAX_EVENTS="$OPTARG" ;;
    s) START_LEDGER="$OPTARG" ;;
    h|*)
      sed -n '2,/^set/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
  esac
done

command -v "$JQ" >/dev/null || { echo "jq not found" >&2; exit 127; }
command -v "$CURL" >/dev/null || { echo "curl not found" >&2; exit 127; }

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
FIXTURES_ROOT="$REPO_ROOT/test/fixtures/reflector"
mkdir -p "$FIXTURES_ROOT"

# Pre-encoded Symbol("REFLECTOR") / Symbol("update"), the decoder's
# classification constants. Hand-written to avoid a Go build step; keep in sync
# with internal/scval/scval_test.go TestGolden_symbolBytes.
TOPIC0='AAAADwAAAAlSRUZMRUNUT1IAAAA='
TOPIC1='AAAADwAAAAZ1cGRhdGUAAA=='

rpc_call() {
  local method="$1"
  local params="$2"
  "$CURL" -sS -X POST "$ENDPOINT" \
    -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$method\",\"params\":$params}"
}

if [[ -z "$START_LEDGER" ]]; then
  latest_resp="$(rpc_call getLatestLedger '{}')"
  LATEST="$(echo "$latest_resp" | "$JQ" -r '.result.sequence')"
  [[ "$LATEST" == "null" || -z "$LATEST" ]] && { echo "getLatestLedger failed: $latest_resp" >&2; exit 1; }
  START_LEDGER=$((LATEST - 200))
  echo "latest ledger: $LATEST → starting from $START_LEDGER"
fi

# Pin the fixture to a contract version. Deriving the hash needs an
# ScAddress-encoded getLedgerEntries key, so the script takes WASM_HASH instead.
WASM_HASH="${WASM_HASH:-unknown-wasm-hash}"
if [[ "$WASM_HASH" == "unknown-wasm-hash" ]]; then
  echo "note: WASM_HASH env var not set — fixtures will land under /unknown-wasm-hash/." >&2
  echo "      to pin: WASM_HASH=<hex> scripts/dev/capture-reflector-fixtures.sh …" >&2
fi
OUT_DIR="$FIXTURES_ROOT/$WASM_HASH"
mkdir -p "$OUT_DIR"

# Events for REFLECTOR/update, scoped to the contract. The third topic slot is
# a wildcard: real events carry topic[2] = u64 timestamp (ms), and stellar-rpc's
# length-aware filter drops events with a longer topic array unless the tail is
# "*" (WildCardExactOne, go-stellar-sdk/protocols/rpc/get_events.go:21).
params="$("$JQ" -nc \
  --arg c "$CONTRACT_ID" \
  --arg t0 "$TOPIC0" \
  --arg t1 "$TOPIC1" \
  --argjson start "$START_LEDGER" \
  --argjson limit "$MAX_EVENTS" '{
    startLedger: $start,
    filters: [{
      type: "contract",
      contractIds: [$c],
      topics: [[$t0, $t1, "*"]]
    }],
    pagination: {limit: $limit}
  }')"

resp="$(rpc_call getEvents "$params")"

err="$(echo "$resp" | "$JQ" -r '.error // empty')"
[[ -n "$err" ]] && { echo "getEvents error: $err" >&2; exit 1; }

count="$(echo "$resp" | "$JQ" '.result.events | length')"
echo "captured $count events for $CONTRACT_ID from ledger $START_LEDGER"
[[ "$count" == "0" ]] && exit 0

echo "$resp" | "$JQ" -c '.result.events[]' | while read -r evt; do
  ledger="$(echo "$evt" | "$JQ" -r '.ledger')"
  tx="$(echo "$evt" | "$JQ" -r '.txHash')"
  closed="$(echo "$evt" | "$JQ" -r '.ledgerClosedAt')"
  topics="$(echo "$evt" | "$JQ" -c '.topic // .topics')"
  # `value` is a base64 ScVal string; older protocol versions wrapped it as
  # {xdr: "..."}. Tolerate both.
  value="$(echo "$evt" | "$JQ" -r 'if (.value|type) == "object" then .value.xdr else .value end')"

  out="$OUT_DIR/${ledger}_${tx:0:12}.json"
  "$JQ" -n \
    --arg c "$CONTRACT_ID" \
    --arg w "$WASM_HASH" \
    --argjson l "$ledger" \
    --arg t "$tx" \
    --arg ts "$closed" \
    --argjson tp "$topics" \
    --arg v "$value" \
    '{
      contract_id: $c,
      wasm_hash: $w,
      ledger: $l,
      tx_hash: $t,
      ledger_closed_at: $ts,
      topics: $tp,
      value: $v
    }' > "$out"
  echo "  wrote $out"
done

echo "done → $OUT_DIR"
