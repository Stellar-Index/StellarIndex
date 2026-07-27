#!/usr/bin/env bash
# route-sweep.sh — hit EVERY GET route in the OpenAPI spec and report its
# status, so "which surfaces actually work" is evidence rather than belief.
#
# Why this exists: on 2026-07-27 the explorer's core routes
# (/v1/accounts/{addr}, /v1/ledgers, /v1/contracts) were found returning
# 503 in production — invisible to every existing check, because
# r1-smoke.sh covers 13 hand-picked GETs and the SLA probe covers the
# pricing path. Neither touches the explorer. A per-route sweep is the
# only thing that catches a whole subsystem being dark.
#
# Path params are filled from a small fixture table of REAL mainnet
# identifiers (below) so a 404 means "route broken", not "made-up id".
# Routes whose params cannot be auto-filled are reported SKIP, never
# silently dropped — an unswept route is exactly how this class hides.
#
# Read-only. Exit code = number of 5xx responses (capped 255).
set -o pipefail  # NOT -u: the FIX lookup uses ${FIX[k]:-} on a possibly-absent key

# NOTE: spec paths are relative to servers[].url, which already carries
# the /v1 prefix — so the base must include it. Omitting it makes every
# route 404 and looks like a total outage (observed while writing this).
API="${API_BASE_URL:-https://api.stellarindex.io/v1}"
SPEC="${SPEC:-openapi/stellar-index.v1.yaml}"

# Real mainnet fixtures — keep these valid; a stale fixture turns a
# healthy route into a false 404.
declare -A FIX=(
  [asset_id]="USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
  [slug]="usdc"
  [address]="GA3GJGKCUKPOPL6NYPMSBK7LMFYNW7SJMAJ7ZGWR3KGSHJWJHQRQZA3L"
  [account_id]="GA3GJGKCUKPOPL6NYPMSBK7LMFYNW7SJMAJ7ZGWR3KGSHJWJHQRQZA3L"
  [contract_id]="CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
  [ledger]="63670000"
  [sequence]="63670000"
  [base]="native"
  [quote]="fiat:USD"
  [pair]="native_fiat:USD"
  [source]="soroswap"
  [name]="soroswap"
  [protocol]="soroswap"
  [code]="USDC"
  [issuer]="GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
  [id]="1"
  [hash]="28e04707f14aa2082a1dc66dcaf73be9107bc3b27bd3dd3281fb4de424793360"
  [tx_hash]="28e04707f14aa2082a1dc66dcaf73be9107bc3b27bd3dd3281fb4de424793360"
)

python3 - "$SPEC" > /tmp/route-sweep-paths.txt <<'PY'
import sys, yaml
spec = yaml.safe_load(open(sys.argv[1]))
for p, ops in spec.get('paths', {}).items():
    if 'get' in ops:
        print(p)
PY

printf '# route sweep — %s\n# api=%s\n\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$API"
printf '%-6s %-8s %s\n' STATUS VERDICT ROUTE

fivexx=0; skipped=0; ok=0; clienterr=0
while read -r route; do
  [ -z "$route" ] && continue
  filled="$route"
  unresolved=0
  while [[ "$filled" =~ \{([a-zA-Z_]+)\} ]]; do
    key="${BASH_REMATCH[1]}"
    val="${FIX[$key]:-}"
    if [ -z "$val" ]; then unresolved=1; break; fi
    filled="${filled//\{$key\}/$val}"
  done
  if [ "$unresolved" = "1" ]; then
    printf '%-6s %-8s %s\n' "-" "SKIP" "$route (no fixture for a path param)"
    skipped=$((skipped+1)); continue
  fi
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 25 "${API}${filled}")
  case "$code" in
    5*) verdict="FAIL"; fivexx=$((fivexx+1)) ;;
    4*) verdict="CLIENT"; clienterr=$((clienterr+1)) ;;
    *)  verdict="ok"; ok=$((ok+1)) ;;
  esac
  printf '%-6s %-8s %s\n' "$code" "$verdict" "$filled"
  sleep 0.15
done < /tmp/route-sweep-paths.txt

echo
echo "ok=${ok} client_4xx=${clienterr} server_5xx=${fivexx} skipped=${skipped}"
[ "$fivexx" -gt 255 ] && fivexx=255
exit "$fivexx"
