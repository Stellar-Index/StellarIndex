#!/usr/bin/env bash
# Capture real Spectra events (registry, PT, YT) from a live stellar-rpc.
# Writes one fixture JSON per event under
# test/fixtures/spectra/<wasm_hash>/<kind>_*.json, where <kind> is the
# event's topic[0] symbol (pt_minted, redeem, transfer, ...) and
# <wasm_hash> is the emitting contract's own executable hash, resolved
# via getLedgerEntries (the registry, PTs and YTs have different hashes).
#
# Needs python3 (strkey decode + instance-key encode) beside jq/curl.
# stellar-rpc is for fixture capture only (AGENTS.md invariant 6).
#
# Usage:
#   scripts/dev/capture-spectra-fixtures.sh \
#     [-e https://mainnet.sorobanrpc.com] [-n 1] [-s <start_ledger>]
#
# Flags:
#   -e  stellar-rpc endpoint (default: http://127.0.0.1:8000, or $ENDPOINT).
#   -n  Max events per (contract, kind) (default: 1).
#   -s  Start ledger (default: the RPC's oldest retained ledger).
#
# Environment:
#   WASM_HASH   Fallback directory label when a contract's hash cannot be
#               resolved (default: "unknown-wasm-hash").
#   MAX_PAGES   getEvents pages per contract (default: 40).
#
# Contract ids: PT/YT/IBT are from Spectra's operator API
# (app.spectra.finance/api/v1/stellar/pools); the registry from
# docs/protocols/spectra.md. Factory and LimitOrderEngine ids are
# unpublished, so pt_deployed and order events are not captured here.

set -euo pipefail

ENDPOINT="${ENDPOINT:-http://127.0.0.1:8000}"
MAX_EVENTS="${MAX_EVENTS:-1}"
MAX_PAGES="${MAX_PAGES:-40}"
START_LEDGER=""
JQ="${JQ:-jq}"
CURL="${CURL:-curl}"
WASM_HASH="${WASM_HASH:-unknown-wasm-hash}"

# role|contract|market decimals. IBT wrappers are included for their
# own transfer/mint/burn shape.
CONTRACTS="
registry|CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V|
pt|CAAOR5F43GSQZYJESHIVLGZBMHMH3UVJMSBEHFKCUBKOUQSZMQC5UCMK|7
yt|CBDQZFWY735RH3PQLNK4BIOO7DTJY4ZQWK5YDFCZ7EIL5DAO2567TORX|7
ibt|CBRT4E5AH23GMRQI7H6HQW54HMDMK4C23OO2CEN5OHWEOSRYBQZCMYBC|7
pt|CA7KTCVDJXQBC7PB6CANFEBPKAQUGVG6SIZVHGFPQCDX3APPTSROJ6AS|7
yt|CDNNGBZOH2OOJ2OKON3A3IVD2JS323BAI2DX55JEWKD327XBJDYDUPAR|7
ibt|CCECATRPUHLMFTIUQDQQPOU5GLXQGJYGBJHKSFMWJYOFEIFNN3MOQWU3|7
pt|CD5YZRFQCATFOFZPWE4XDYJZMXAZSW5ROTIAO4D65Q7KTMZIEWGB7H7W|13
yt|CAUKKZSHKNCP44C2CLNNDMI4H5JF4EJUJ7CRZR2Z7NTLI2N6BE2SRBYP|13
pt|CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN|13
yt|CCZVSODQH6UVMSENOAPBGVH3ZHMUSM5OXZJGMRYIO2V22HPI56U6PBFL|13
pt|CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ|18
yt|CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G|18
pt|CAAQJ6CN3KWJUG2CTUFUV27IL2BBWEIQKQA27HR7KFFZXROLHN6ELMBI|18
yt|CBRI2RSGUJJOTP3W7HLICWOFHCDB64S73EIUYU653JG7GKZRTQDT72V7|18
pt|CDHIBKKS53XQAMIDVL7SZLPM2DEHH3OCO7SU6IE3OW65LESYF5K5ABMU|18
yt|CAQPNK37HP3BVDNYNYJ7USCFCLXO4S7K75VSLF54BD25APTHSM5COT5L|18
ibt|CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO|18
"

# shellcheck source=scripts/dev/lib/fixture-capture-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/fixture-capture-common.sh"

fixture_capture_parse_args "$@"
fixture_capture_check_deps
command -v python3 >/dev/null || { echo "python3 not found" >&2; exit 127; }
fixture_capture_setup_outdir spectra
FIXTURE_ROOT="$(dirname "$OUT_DIR")"
rmdir "$OUT_DIR" 2>/dev/null || true

# rpc_retry is rpc() retried while the reply is not JSON (public RPCs
# answer rate limits with plain text).
rpc_retry() {
  local out try
  for try in 1 2 3 4 5; do
    out="$(rpc "$1" "$2" || true)"
    if echo "$out" | "$JQ" -e . >/dev/null 2>&1; then
      echo "$out"
      return 0
    fi
    sleep $((try * 2))
  done
  echo "$1: no JSON reply after retries: ${out:0:80}" >&2
  return 1
}

if [[ -z "$START_LEDGER" ]]; then
  START_LEDGER="$(rpc_retry getHealth '{}' | "$JQ" -r '.result.oldestLedger')"
  [[ "$START_LEDGER" =~ ^[0-9]+$ ]] || { echo "getHealth failed" >&2; exit 1; }
  # Retention edge can be evicted between the two calls.
  START_LEDGER=$((START_LEDGER + 100))
  echo "starting from oldest retained ledger + 100: $START_LEDGER"
fi

# resolve_wasm_hash prints the hex executable hash of a contract
# instance, or nothing when it cannot be resolved.
resolve_wasm_hash() {
  local key resp
  key="$(python3 - "$1" <<'PY'
import base64, struct, sys
s = sys.argv[1]
raw = base64.b32decode(s + "=" * (-len(s) % 8))
h = raw[1:33]  # version byte, 32-byte hash, 2-byte crc
# LedgerKey CONTRACT_DATA(6): SC_ADDRESS_TYPE_CONTRACT(1)+hash,
# key SCV_LEDGER_KEY_CONTRACT_INSTANCE(20), durability PERSISTENT(1).
print(base64.b64encode(struct.pack(">II", 6, 1) + h + struct.pack(">II", 20, 1)).decode())
PY
)" || return 0
  resp="$(rpc_retry getLedgerEntries "{\"keys\":[\"$key\"]}")"
  echo "$resp" | "$JQ" -r '.result.entries[0].xdr // empty' | python3 -c '
import base64, sys
b = base64.b64decode(sys.stdin.read().strip() or "")
# entry data disc(4) ext(4) addr(36) key disc(4) dur(4) val disc(4) exec disc(4) hash(32)
if len(b) >= 92 and b[:4] == bytes([0, 0, 0, 6]) and b[52:56] == bytes([0, 0, 0, 19]) and b[56:60] == bytes(4):
    print(b[60:92].hex())
'
}

captured=0
while IFS='|' read -r role contract decimals; do
  [[ -z "$role" ]] && continue
  hash="$(resolve_wasm_hash "$contract")"
  [[ -z "$hash" ]] && hash="$WASM_HASH"
  dir="$FIXTURE_ROOT/$hash"
  mkdir -p "$dir"

  events="$(mktemp)"
  cursor=""
  for ((page = 0; page < MAX_PAGES; page++)); do
    if [[ -z "$cursor" ]]; then
      # shellcheck disable=SC2016  # $s/$c are jq variables, not shell
      params="$("$JQ" -nc --argjson s "$START_LEDGER" --arg c "$contract" \
        '{startLedger:$s, filters:[{type:"contract", contractIds:[$c]}], pagination:{limit:1000}}')"
    else
      # shellcheck disable=SC2016
      params="$("$JQ" -nc --arg k "$cursor" --arg c "$contract" \
        '{filters:[{type:"contract", contractIds:[$c]}], pagination:{cursor:$k, limit:1000}}')"
    fi
    resp="$(rpc_retry getEvents "$params")" || break
    err="$(echo "$resp" | "$JQ" -r '.error // empty')"
    if [[ -n "$err" ]]; then
      echo "$role $contract: getEvents error: $err" >&2
      break
    fi
    echo "$resp" | "$JQ" -c '.result.events[]' >> "$events"
    # An empty page is not the end: one request scans a bounded ledger span.
    next="$(echo "$resp" | "$JQ" -r '.result.cursor // empty')"
    [[ -z "$next" || "$next" == "$cursor" ]] && break
    cursor="$next"
    latest="$(echo "$resp" | "$JQ" -r '.result.latestLedger')"
    [[ $((10#${cursor%%-*} >> 32)) -ge "$latest" ]] && break
  done

  # kind = topic[0] symbol: XDR ScVal Symbol = 4-byte type, 4-byte length, name, zero pad.
  # shellcheck disable=SC2016  # $r/$d/$w/$m/$n are jq variables, not shell
  "$JQ" -c --arg r "$role" --arg d "$decimals" --arg w "$hash" --argjson m "$MAX_EVENTS" '
    . + {kind: (.topic[0] | @base64d | .[8:] | gsub("\u0000"; ""))}
  ' "$events" | "$JQ" -cs --argjson m "$MAX_EVENTS" '
    group_by(.kind)[] | .[:$m][]' | while read -r evt; do
    kind="$(echo "$evt" | "$JQ" -r '.kind')"
    ledger="$(echo "$evt" | "$JQ" -r '.ledger')"
    tx="$(echo "$evt" | "$JQ" -r '.txHash')"
    fname="$dir/${kind}_${role}_${ledger}_${tx:0:12}_${contract:0:6}.json"
    # shellcheck disable=SC2016
    echo "$evt" | "$JQ" \
      --arg r "$role" --arg d "$decimals" --arg w "$hash" '
      { contract_id: .contractId, role: $r, market_decimals: (if $d == "" then null else ($d|tonumber) end),
        wasm_hash: $w, ledger: .ledger, tx_hash: .txHash,
        ledger_closed_at: .ledgerClosedAt, topics: (.topic // .topics),
        value: (if (.value|type) == "object" then .value.xdr else .value end),
        event_name: .kind }' > "$fname"
    echo "  wrote ${fname#"$FIXTURE_ROOT"/}"
  done
  total="$(wc -l < "$events" | tr -d ' ')"
  echo "$role $contract ($hash): $total event(s) in window"
  rm -f "$events"
  captured=$((captured + 1))
done <<< "$CONTRACTS"

echo "done → $FIXTURE_ROOT ($captured contracts)"
