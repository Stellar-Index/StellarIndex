#!/usr/bin/env bash
# route-sweep.sh — hit EVERY GET route in the OpenAPI spec and report its
# status, so "which surfaces actually work" is evidence rather than belief.
#
# Why this exists: on 2026-07-27 the explorer's core routes
# (/v1/accounts/{addr}, /v1/ledgers, /v1/contracts) were found returning
# 503 in production — invisible to every existing check, because
# r1-smoke.sh covers a hand-picked set of GETs and the SLA probe covers the
# pricing path. Neither touches the explorer. A per-route sweep is the
# only thing that catches a whole subsystem being dark.
#
# Path params are filled from a small fixture table of REAL mainnet
# identifiers (below) so a 404 means "route broken", not "made-up id".
# Routes whose params cannot be auto-filled are reported SKIP, never
# silently dropped — an unswept route is exactly how this class hides.
#
# Read-only. Exit code = number of unreachable + 5xx responses (capped 255):
# a route curl could not even connect to (DNS/TLS/connection/timeout →
# http_code 000) is a FAILURE, not a pass — "couldn't connect" must never
# score the same as "connected and returned 2xx" (W5-ci-3).
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

# A tooling failure here (missing PyYAML, an unreadable/malformed spec,
# a generator that silently matches nothing) must never read as "every
# route healthy" — that is exactly the shape of the 2026-07-27 incident
# this script exists to catch, just moved one layer down into the tool
# itself. So the generator's own exit status is checked (a heredoc's
# exit code IS the interpreter's, since there is no pipe in front of
# it), and the resulting list is required to clear a floor AND contain
# routes from the exact 2026-07-27 outage (/ledgers, /contracts,
# /accounts/{g_strkey}) before a single curl is issued. Any of these
# failing is a REFUSAL (exit 2), not a zero-route clean sweep.
ROUTE_SWEEP_MIN_ROUTES="${ROUTE_SWEEP_MIN_ROUTES:-50}"
ROUTE_SWEEP_KNOWN_ROUTES=(/ledgers /contracts "/accounts/{g_strkey}")

# generate_route_list SPEC OUT — writes every GET path from SPEC into
# OUT, one per line. Returns non-zero (and leaves a message on stderr)
# if the parser failed, or if the result doesn't look like a real spec.
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
  # A route curl could not reach — DNS/TLS/connection error or timeout —
  # reports http_code 000 (and an empty/non-numeric status if the -w write
  # itself produced nothing). That is a reachability FAILURE, not a pass:
  # scoring "couldn't connect" as ok is exactly how a dark subsystem hides
  # (W5-ci-3). Only an actually-reachable response falls through to the
  # 2xx/3xx=ok, 4xx=client, 5xx=fail verdicts below.
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
# Exit = failures the operator must act on: server errors AND unreachable
# routes. A single reachable-but-broken sweep and a total-outage sweep must
# never both read as exit 0.
failures=$((fivexx + unreach))
[ "$failures" -gt 255 ] && failures=255
exit "$failures"
}

# Sourced by route-sweep-test.sh to exercise generate_route_list without
# running the network sweep; only runs the sweep when executed directly.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  route_sweep_main
fi
