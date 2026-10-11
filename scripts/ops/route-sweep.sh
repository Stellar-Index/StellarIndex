#!/usr/bin/env bash
# route-sweep.sh — hit EVERY GET route in the OpenAPI spec and report its
# status, so "which surfaces actually work" is evidence rather than belief.
#
# Why: the explorer's core routes (/v1/accounts/{addr}, /v1/ledgers,
# /v1/contracts) were found returning 503 in production, invisible to every
# existing check: r1-smoke.sh covers a hand-picked set of GETs and the SLA
# probe covers the pricing path. Only a per-route sweep catches a whole
# subsystem being dark.
#
# Path params are filled from a table of REAL mainnet identifiers so a 404
# means "route broken", not "made-up id". Routes whose params cannot be filled
# are reported SKIP, never silently dropped.
#
# Read-only. Exit code = unreachable + 5xx responses (capped 255). A route
# curl could not connect to (DNS/TLS/connection/timeout → http_code 000) is a
# FAILURE, never scored like a 2xx.
set -o pipefail  # NOT -u: fixture_for returns empty for unmapped params

# Spec paths are relative to servers[].url, which already carries /v1, so the
# base must include it; omitting it makes every route 404 like a total outage.
API="${API_BASE_URL:-https://api.stellarindex.io/v1}"
SPEC="${SPEC:-openapi/stellar-index.v1.yaml}"

# Real mainnet fixtures; a stale fixture turns a healthy route into a false
# 404. These are the parameter names the spec uses (derived from it):
#   asset_id contract_id entity_type g_strkey hash id keyID name pool seq slug
# An unmapped name makes the route SKIP; a WRONGLY-typed value makes a false 4xx.
# A `case` lookup, NOT an associative array: macOS bash 3.2 silently degrades
# `declare -A` to an INDEXED array, collapsing every `[key]=` onto index 0, so
# every lookup returns the LAST value and the 4xx column is noise.
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

# A tooling failure (missing PyYAML, malformed spec, a generator matching
# nothing) must never read as "every route healthy": that is the route-503
# incident's shape one layer down. So the generator's exit status is checked
# (a heredoc's exit code is the interpreter's, no pipe in front), and the list
# must clear a floor AND contain routes from that outage (/ledgers, /contracts,
# /accounts/{g_strkey}) before any curl is issued. Failure is a REFUSAL
# (exit 2), not a zero-route clean sweep.
ROUTE_SWEEP_MIN_ROUTES="${ROUTE_SWEEP_MIN_ROUTES:-50}"
ROUTE_SWEEP_KNOWN_ROUTES=(/ledgers /contracts "/accounts/{g_strkey}")

# generate_route_list SPEC OUT — writes every GET path from SPEC into OUT, one
# per line. Non-zero (message on stderr) if the parser failed or the result
# does not look like a real spec.
generate_route_list() {
  local spec="$1" out="$2" n known
  if ! python3 - "$spec" > "$out" <<'PY'
import sys, yaml
spec = yaml.safe_load(open(sys.argv[1]))
for p, ops in spec.get('paths', {}).items():
    if 'get' in ops:
        print(p)
PY
  then
    echo "route-sweep: spec parser failed on '$spec' (python3 exited non-zero — see its stderr above); refusing to report a clean sweep" >&2
    return 1
  fi
  n=$(wc -l < "$out" | tr -d ' ')
  if [ -z "$n" ] || [ "$n" -lt "$ROUTE_SWEEP_MIN_ROUTES" ]; then
    echo "route-sweep: parser produced ${n:-0} route(s) from '$spec', below the floor of $ROUTE_SWEEP_MIN_ROUTES (set ROUTE_SWEEP_MIN_ROUTES to override) — this is a tooling failure, not a clean sweep" >&2
    return 1
  fi
  for known in "${ROUTE_SWEEP_KNOWN_ROUTES[@]}"; do
    if ! grep -qxF "$known" "$out"; then
      echo "route-sweep: expected route '$known' (present the day of the 2026-07-27 outage) is missing from the parsed spec — this is a tooling failure, not a clean sweep" >&2
      return 1
    fi
  done
  return 0
}

route_sweep_main() {
if ! generate_route_list "$SPEC" /tmp/route-sweep-paths.txt; then
  exit 2
fi

printf '# route sweep — %s\n# api=%s\n\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$API"
printf '%-6s %-8s %s\n' STATUS VERDICT ROUTE

fivexx=0; skipped=0; ok=0; clienterr=0; unreach=0
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
  # A route curl could not reach (DNS/TLS/connection error, timeout) reports
  # http_code 000, or an empty/non-numeric code if -w wrote nothing. That is a
  # reachability FAILURE: scoring it ok is how a dark subsystem hides. Only a
  # reachable response falls through to the verdicts below.
  if ! [[ "$code" =~ ^[0-9]{3}$ ]] || [ "$code" = "000" ]; then
    printf '%-6s %-8s %s\n' "${code:-000}" "UNREACH" "$filled (curl could not connect)"
    unreach=$((unreach+1)); sleep 0.15; continue
  fi
  case "$code" in
    5*) verdict="FAIL"; fivexx=$((fivexx+1)) ;;
    4*) verdict="CLIENT"; clienterr=$((clienterr+1)) ;;
    *)  verdict="ok"; ok=$((ok+1)) ;;
  esac
  printf '%-6s %-8s %s\n' "$code" "$verdict" "$filled"
  sleep 0.15
done < /tmp/route-sweep-paths.txt

echo
echo "ok=${ok} client_4xx=${clienterr} server_5xx=${fivexx} unreachable=${unreach} skipped=${skipped}"
# Exit = failures the operator must act on: 5xx AND unreachable routes, so a
# broken sweep and a total outage never both read as 0.
failures=$((fivexx + unreach))
[ "$failures" -gt 255 ] && failures=255
exit "$failures"
}

# Sourced by route-sweep-test.sh to exercise generate_route_list without
# running the network sweep; only runs the sweep when executed directly.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  route_sweep_main
fi
