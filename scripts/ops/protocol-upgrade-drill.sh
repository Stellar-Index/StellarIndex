#!/usr/bin/env bash
# protocol-upgrade-drill.sh — compare a GREEN stack against the LIVE stack on a
# list of golden ledgers and print a pass/fail diff report, BEFORE green serves.
#
# Golden ledgers are the ones that exercise a protocol change on a test net
# (the upgrade ledger itself, the first ledger after it, ledgers holding the new
# operation/XDR shapes). Both stacks must already have ingested them; the drill
# reads only the public explorer routes, so it needs no ledger-lake access.
#
# For each ledger it fetches /ledgers/{seq}, /ledgers/{seq}/transactions and
# /ledgers/{seq}/operations from both bases, normalises away request-time
# fields (as_of, flags), and diffs the rest. It also requires every GREEN header
# to report protocol_version >= DRILL_MIN_PROTOCOL when that is set.
#
# Usage:
#   LIVE_URL=https://testnet.example/v1 GREEN_URL=http://10.0.0.2:8080/v1 \
#   DRILL_MIN_PROTOCOL=29 bash scripts/ops/protocol-upgrade-drill.sh golden.txt
#
# golden.txt: one ledger sequence per line; blank lines and #-comments ignored.
# Exit code = number of FAILed checks (capped 255). An unreachable base, an
# HTTP error on either side, a truncated list or an unparsable body is a FAIL, never a pass: the
# drill must not read "could not compare" as "identical". Exit 2 = refused to
# run (missing bases, missing/empty/malformed golden list).
set -o pipefail

# Maximum page sizes: a first-page-only comparison would pass a divergence later in the ledger.
DRILL_ROUTES=(/ledgers/%s "/ledgers/%s/transactions?limit=1000" "/ledgers/%s/operations?limit=2000")

# normalize_json — stdin JSON → canonical text without request-time fields.
normalize_json() {
  python3 -c '
import json, sys
d = json.load(sys.stdin)
if isinstance(d, dict):
    d.pop("as_of", None)
    d.pop("flags", None)
print(json.dumps(d, sort_keys=True, indent=1))
'
}

# is_truncated FILE — true when the normalised body reports data.truncated.
is_truncated() {
  python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
inner = d.get("data") if isinstance(d, dict) else None
sys.exit(0 if isinstance(inner, dict) and inner.get("truncated") is True else 1)
' "$1"
}

# fetch_norm BASE PATH OUT — writes the normalised body to OUT and echoes the
# HTTP status (000 on a transport failure, BADJSON on an unparsable 200,
# TRUNCATED when the body says the list was cut short).
fetch_norm() {
  local base="$1" path="$2" out="$3" raw code
  raw="$(mktemp)"
  code="$(curl -s -o "$raw" -w '%{http_code}' --max-time 60 "${base}${path}")"
  [[ "$code" =~ ^[0-9]{3}$ ]] || code=000
  if [ "$code" = 200 ] && ! normalize_json < "$raw" > "$out" 2>/dev/null; then
    code=BADJSON
  elif [ "$code" = 200 ] && is_truncated "$out"; then
    code=TRUNCATED
  fi
  [ "$code" = 200 ] || : > "$out"
  rm -f "$raw"
  echo "$code"
}

# green_protocol FILE — protocol_version from a normalised ledger header.
green_protocol() {
  python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print((d.get("data") or d).get("protocol_version", ""))
' "$1" 2>/dev/null
}

# read_golden FILE — prints validated sequences; non-zero on a bad list.
read_golden() {
  local golden="$1" seqs bad
  if [ -z "$golden" ] || [ ! -r "$golden" ]; then
    echo "drill: golden ledger list '${golden}' is missing or unreadable" >&2; return 1
  fi
  seqs="$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "$golden" | grep -v '^$')"
  if [ -z "$seqs" ]; then
    echo "drill: golden list '$golden' has no ledgers; refusing an empty pass" >&2; return 1
  fi
  bad="$(printf '%s\n' "$seqs" | grep -v -E '^[0-9]+$')"
  if [ -n "$bad" ]; then
    bad="${bad%%$'\n'*}"
    echo "drill: '$bad' in $golden is not a ledger sequence" >&2; return 1
  fi
  printf '%s\n' "$seqs"
}

# check_route SEQ PATH TMPDIR — prints the verdict line(s); echoes nothing else.
# Returns the number of failed checks (0-2).
check_route() {
  local seq="$1" path="$2" tmp="$3" lc gc pv f=0
  local lf="$tmp/live.json" gf="$tmp/green.json"
  lc="$(fetch_norm "$LIVE_URL" "$path" "$lf")"
  gc="$(fetch_norm "$GREEN_URL" "$path" "$gf")"
  if [ "$lc" != 200 ] || [ "$gc" != 200 ]; then
    printf '%-7s %-12s %s (live=%s green=%s)\n' FAIL "$seq" "$path" "$lc" "$gc"
    return 1
  fi
  if cmp -s "$lf" "$gf"; then
    printf '%-7s %-12s %s\n' PASS "$seq" "$path"
  else
    printf '%-7s %-12s %s\n' FAIL "$seq" "$path"
    diff -u --label live --label green "$lf" "$gf" | sed -n '1,40s/^/        /p'
    f=$((f + 1))
  fi
  if [ -n "${DRILL_MIN_PROTOCOL:-}" ] && [ "$path" = "/ledgers/$seq" ]; then
    pv="$(green_protocol "$gf")"
    if [[ "$pv" =~ ^[0-9]+$ ]] && [ "$pv" -ge "$DRILL_MIN_PROTOCOL" ]; then
      printf '%-7s %-12s green protocol_version=%s >= %s\n' PASS "$seq" "$pv" "$DRILL_MIN_PROTOCOL"
    else
      printf '%-7s %-12s green protocol_version=%s < %s\n' FAIL "$seq" "${pv:-?}" "$DRILL_MIN_PROTOCOL"
      f=$((f + 1))
    fi
  fi
  return "$f"
}

drill_main() {
  if [ -z "${LIVE_URL:-}" ] || [ -z "${GREEN_URL:-}" ]; then
    echo "drill: LIVE_URL and GREEN_URL are required (bases including /v1)" >&2; return 2
  fi
  local seqs tmp fail=0 checks=0 seq tpl rc
  seqs="$(read_golden "${1:-}")" || return 2
  tmp="$(mktemp -d)"
  printf '# protocol-upgrade drill — %s\n# live=%s\n# green=%s\n\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$LIVE_URL" "$GREEN_URL"
  printf '%-7s %-12s %s\n' VERDICT LEDGER CHECK
  while read -r seq; do
    for tpl in "${DRILL_ROUTES[@]}"; do
      checks=$((checks + 1))
      check_route "$seq" "${tpl//%s/$seq}" "$tmp"; rc=$?
      fail=$((fail + rc))
    done
  done <<< "$seqs"
  rm -rf "$tmp"
  echo
  echo "routes=${checks} fail=${fail}"
  [ "$fail" -gt 255 ] && fail=255
  return "$fail"
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  drill_main "$@"
  exit $?
fi
