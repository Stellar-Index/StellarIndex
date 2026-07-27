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
set -o pipefail  # NOT -u: fixture_for returns empty for unmapped params

# NOTE: spec paths are relative to servers[].url, which already carries
# the /v1 prefix — so the base must include it. Omitting it makes every
# route 404 and looks like a total outage (observed while writing this).
API="${API_BASE_URL:-https://api.stellarindex.io/v1}"
SPEC="${SPEC:-openapi/stellar-index.v1.yaml}"

# Real mainnet fixtures — keep these valid; a stale fixture turns a
# healthy route into a false 404.
# These are the ELEVEN parameter names the spec actually uses, derived
# from the spec itself rather than guessed:
#   asset_id contract_id entity_type g_strkey hash id keyID name pool seq slug
# An unmapped name would make the route SKIP; a WRONGLY-typed value makes
# it a false 4xx, which is why the first run's 4xx tally was untrustworthy.
# A `case` lookup, NOT an associative array. macOS ships bash 3.2, where
# `declare -A` silently degrades to an INDEXED array: every `[key]=`
# subscript is evaluated arithmetically, undefined names become 0, so all
# entries collapse onto index 0 and every lookup returns the LAST value.
# The sweep then requests the same nonsense id for every route and the
# 4xx column is pure noise — which is exactly what happened on the first
# two runs (2026-07-28) before this was caught. `case` works everywhere.
#
# Per-key rationale:
#   pool        - a Blend/Phoenix pool contract id
#   entity_type - enum on /changes/{entity_type}/{id}; "account" pairs
#                 with a real G-strkey to actually exercise the route
#   keyID       - only on /account/keys + /admin/keys, which are
#                 auth-scoped, so 401/403 there is CORRECT, not a defect
fixture_for() {
  case "$1" in
    asset_id)    echo "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" ;;
    slug)        echo "usdc" ;;
    g_strkey)    echo "GA3GJGKCUKPOPL6NYPMSBK7LMFYNW7SJMAJ7ZGWR3KGSHJWJHQRQZA3L" ;;
    contract_id) echo "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32" ;;
    seq)         echo "63670000" ;;
    hash)        echo "28e04707f14aa2082a1dc66dcaf73be9107bc3b27bd3dd3281fb4de424793360" ;;
    name)        echo "soroswap" ;;
    pool)        echo "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32" ;;
    entity_type) echo "account" ;;
    id)          echo "GA3GJGKCUKPOPL6NYPMSBK7LMFYNW7SJMAJ7ZGWR3KGSHJWJHQRQZA3L" ;;
    keyID)       echo "sip_placeholder_expected_401" ;;
    *)           echo "" ;;
  esac
}

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
    val="$(fixture_for "$key")"
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
