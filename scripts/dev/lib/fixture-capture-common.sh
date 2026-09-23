#!/usr/bin/env bash
# Shared getopts/dependency-check/output-dir/rpc machinery for the
# fixture-capture dev scripts (capture-aquarius-fixtures.sh,
# capture-soroswap-fixtures.sh). Sourced, not executed directly.

# fixture_capture_parse_args parses -e/-n/-s/-h into ENDPOINT, MAX_EVENTS
# and START_LEDGER; -h (or an unknown flag) prints $0's header comment
# and exits 0.
fixture_capture_parse_args() {
  while getopts "e:n:s:h" opt; do
    # shellcheck disable=SC2034  # MAX_EVENTS is consumed by the sourcing script, not this file
    case "$opt" in
      e) ENDPOINT="$OPTARG" ;;
      n) MAX_EVENTS="$OPTARG" ;;
      s) START_LEDGER="$OPTARG" ;;
      h|*)
        sed -n '2,/^set/p' "$0" | sed 's/^# \{0,1\}//'
        exit 0 ;;
    esac
  done
}

# fixture_capture_check_deps verifies jq/curl (as named by $JQ/$CURL)
# are on PATH.
fixture_capture_check_deps() {
  command -v "$JQ" >/dev/null || { echo "jq not found" >&2; exit 127; }
  command -v "$CURL" >/dev/null || { echo "curl not found" >&2; exit 127; }
}

# fixture_capture_setup_outdir sets REPO_ROOT/OUT_DIR for the given
# protocol subpath (e.g. "aquarius", "soroswap") under $WASM_HASH and
# creates OUT_DIR.
fixture_capture_setup_outdir() {
  local protocol="$1"
  REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
  OUT_DIR="$REPO_ROOT/test/fixtures/$protocol/$WASM_HASH"
  mkdir -p "$OUT_DIR"
}

# rpc issues a JSON-RPC POST to $ENDPOINT with method $1 and params $2.
rpc() {
  "$CURL" -sS -X POST "$ENDPOINT" \
    -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}"
}

# fixture_capture_resolve_start_ledger sets START_LEDGER to
# latestLedger-200 via getLatestLedger when it is not already set.
fixture_capture_resolve_start_ledger() {
  if [[ -z "$START_LEDGER" ]]; then
    local latest
    latest="$(rpc getLatestLedger '{}' | "$JQ" -r '.result.sequence')"
    [[ "$latest" == "null" || -z "$latest" ]] && { echo "getLatestLedger failed" >&2; exit 1; }
    START_LEDGER=$((latest - 200))
    echo "latest ledger: $latest → starting from $START_LEDGER"
  fi
}
