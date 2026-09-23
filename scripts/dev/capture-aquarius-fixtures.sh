#!/usr/bin/env bash
# Capture real Aquarius `trade` events from a live stellar-rpc.
# Writes one fixture JSON per event under
# test/fixtures/aquarius/<wasm_hash>/trade_*.json — consumed by
# internal/sources/aquarius/real_fixture_test.go.
#
# Usage:
#   scripts/dev/capture-aquarius-fixtures.sh \
#     [-e http://127.0.0.1:8000] \
#     [-n 10] \
#     [-s <start_ledger>]
#
# Env:
#   WASM_HASH  Directory label for captured fixtures. Use a tag +
#              date like `v2-2026-04-23` until the ops CLI can
#              resolve real WASM hashes.
#
# Why per-WASM layout: docs/architecture/contract-schema-evolution.md.

set -euo pipefail

ENDPOINT="${ENDPOINT:-http://127.0.0.1:8000}"
MAX_EVENTS="${MAX_EVENTS:-10}"
START_LEDGER=""
JQ="${JQ:-jq}"
CURL="${CURL:-curl}"
WASM_HASH="${WASM_HASH:-unknown-wasm-hash}"

# shellcheck source=scripts/dev/lib/fixture-capture-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/fixture-capture-common.sh"

fixture_capture_parse_args "$@"
fixture_capture_check_deps
fixture_capture_setup_outdir aquarius

# Symbol("trade") wire bytes — matches internal/sources/aquarius's
# TopicSymbolTrade. Regenerate with:
#   go run scripts/dev/encode-topics -type symbol trade
TOPIC_TRADE='AAAADwAAAAV0cmFkZQAAAA=='

fixture_capture_resolve_start_ledger

# Trade events carry 4 topics: [Symbol("trade"), Address(token_in),
# Address(token_out), Address(user)]. Without the three trailing
# wildcards, stellar-rpc's length-aware filter drops every event.
# shellcheck disable=SC2016  # $start/$t0/$limit are jq variables, not shell
params="$("$JQ" -nc \
  --argjson start "$START_LEDGER" \
  --argjson limit "$MAX_EVENTS" \
  --arg t0 "$TOPIC_TRADE" '
    { startLedger: $start,
      filters: [{type:"contract", topics:[[$t0, "*", "*", "*"]]}],
      pagination: {limit: $limit} }')"

resp="$(rpc getEvents "$params")"
err="$(echo "$resp" | "$JQ" -r '.error // empty')"
if [[ -n "$err" ]]; then echo "getEvents error: $err" >&2; exit 1; fi

count="$(echo "$resp" | "$JQ" '.result.events | length')"
echo "captured $count trade events from ledger $START_LEDGER"
[[ "$count" == "0" ]] && exit 0

echo "$resp" | "$JQ" -c '.result.events[]' | while read -r evt; do
  ledger="$(echo "$evt" | "$JQ" -r '.ledger')"
  tx="$(echo "$evt" | "$JQ" -r '.txHash')"
  closed="$(echo "$evt" | "$JQ" -r '.ledgerClosedAt')"
  topics="$(echo "$evt" | "$JQ" -c '.topic // .topics')"
  value="$(echo "$evt" | "$JQ" -r 'if (.value|type) == "object" then .value.xdr else .value end')"
  contract="$(echo "$evt" | "$JQ" -r '.contractId')"
  fname="$OUT_DIR/trade_${ledger}_${tx:0:12}.json"
  # shellcheck disable=SC2016  # $c/$w/$l/... are jq variables, not shell
  "$JQ" -n \
    --arg c "$contract" --arg w "$WASM_HASH" \
    --argjson l "$ledger" --arg t "$tx" \
    --arg ts "$closed" --argjson tp "$topics" \
    --arg v "$value" --arg e "trade" \
    '{contract_id: $c, wasm_hash: $w, ledger: $l, tx_hash: $t,
      ledger_closed_at: $ts, topics: $tp, value: $v, event_name: $e}' > "$fname"
  echo "  wrote $fname"
done
echo "done → $OUT_DIR"
