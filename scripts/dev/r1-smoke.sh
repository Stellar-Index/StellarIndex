#!/usr/bin/env bash
#
# r1-smoke.sh - exercise the launch-critical API surface of a deployment (default: R1 internal
# address; set API_BASE_URL elsewhere). Each check runs independently so one run reports every
# break. Exit code is the number of failed checks. Read-only GETs with truncated bodies and no
# auth, so it is safe from cron or a healthcheck timer.
#
# Usage:
#   bash scripts/dev/r1-smoke.sh                       # localhost:3000: r1's API when run on the host
#   API_BASE_URL=https://api.stellarindex.io bash scripts/dev/r1-smoke.sh   # from a laptop
#   ssh root@136.243.90.96 'bash -s' < scripts/dev/r1-smoke.sh
#
# check NAME PATH [-- jq-test] wants HTTP 200; expect_status STATUS NAME PATH [-- jq-test] pins
# a documented 4xx so a regression to a silent 200 fails instead of passing liveness.

set -uo pipefail

API_BASE_URL="${API_BASE_URL:-http://localhost:3000}"
# Per-request timeout: above the 8s server-side ceiling on cold-path handlers, so exceeding it
# means genuinely hung, not a cold hypertable scan (5s false-positived on cold /v1/markets).
TIMEOUT="${SMOKE_TIMEOUT:-10}"
FAILS=0

# ANSI colours, disabled when stdout is not a TTY.
if [ -t 1 ]; then
  GREEN="$(printf '\033[32m')"; RED="$(printf '\033[31m')"; DIM="$(printf '\033[2m')"; OFF="$(printf '\033[0m')"
else
  GREEN=""; RED=""; DIM=""; OFF=""
fi

# check NAME PATH [-- jq-test]: GET under the TIMEOUT budget, assert 200, optionally `jq -e` the body.
check() {
  expect_status 200 "$@"
}

# expect_status STATUS NAME PATH [-- jq-test]: like check, but asserts an arbitrary status.
# STATUS may be a `|`-separated set ("200|404") for routes whose 4xx is data-dependent, so a 5xx
# still fails. The jq-test sees whatever body came back; keep it generic for multi-status.
expect_status() {
  local want_status="$1" name="$2" path="$3"
  shift 3
  local jq_check=""
  local per_check_timeout="$TIMEOUT"
  while [ "${1:-}" != "" ]; do
    case "$1" in
      --)
        shift
        jq_check="$1"
        shift
        ;;
      --timeout)
        shift
        per_check_timeout="$1"
        shift
        ;;
      *)
        # Unknown trailing arg — leave for the caller to debug.
        break
        ;;
    esac
  done

  local body status
  # User-Agent lets obs.HTTPMetrics exclude synthetic traffic from the SLO histograms.
  #
  # URL goes through printf '%s' so shell-special characters in ${path} stay literal. The
  # per-check --timeout flag gives slow data-dependent paths more budget than the global default.
  local url
  url="$(printf '%s%s' "$API_BASE_URL" "$path")"
  # Capture curl's exit code so reset, refused and timeout are distinguishable in the alert body.
  curl_err="$(mktemp)"
  body="$(curl -sS -m "$per_check_timeout" -A "stellarindex-smoke/1" -w "\n%{http_code}" "$url" 2>"$curl_err")" || curl_rc=$?
  curl_rc="${curl_rc:-0}"
  if [ "$curl_rc" -ne 0 ]; then
    case "$curl_rc" in
      28) why="timed out after ${per_check_timeout}s" ;;
      7)  why="connection refused" ;;
      52) why="empty reply from server" ;;
      56) why="receive error (connection reset)" ;;
      *)  why="curl exit ${curl_rc}" ;;
    esac
    printf "  %sFAIL%s %-32s %s%s — %s%s\n" "$RED" "$OFF" "$name" "$DIM" "$why" \
      "$(tr -d '\n' < "$curl_err" | cut -c1-90)" "$OFF"
    rm -f "$curl_err"
    unset curl_rc
    FAILS=$((FAILS + 1))
    return
  fi
  rm -f "$curl_err"
  unset curl_rc
  status="$(echo "$body" | tail -1)"
  body="$(echo "$body" | sed '$d')"

  # Multi-status acceptance: "200|404" matches either.
  local matched=0
  case "|$want_status|" in
    *"|$status|"*) matched=1 ;;
  esac
  if [ "$matched" -ne 1 ]; then
    printf "  %sFAIL%s %-32s %sHTTP %s (want %s) — %s%s\n" \
      "$RED" "$OFF" "$name" "$DIM" "$status" "$want_status" "${body:0:80}" "$OFF"
    FAILS=$((FAILS + 1))
    return
  fi

  if [ -n "$jq_check" ]; then
    if ! echo "$body" | jq -e "$jq_check" >/dev/null 2>&1; then
      printf "  %sFAIL%s %-32s %sjq assertion failed: %s%s\n" \
        "$RED" "$OFF" "$name" "$DIM" "$jq_check" "$OFF"
      FAILS=$((FAILS + 1))
      return
    fi
  fi

  printf "  %sok%s   %-32s %s%s%s\n" "$GREEN" "$OFF" "$name" "$DIM" "${path}" "$OFF"
}

# skip NAME REASON — an explicit, non-failing line for an assertion that
# cannot hold on this network.
skip() {
  printf "  %sSKIP%s %-32s %s%s%s\n" "$DIM" "$OFF" "$1" "$DIM" "$2" "$OFF"
}

# Test nets have no price sources (protocol sources are anchored to
# pubnet contracts, ADR-0035), so price and verified-asset assertions
# cannot hold there. /v1/coverage reports the served network; if it
# cannot be read, assume pubnet so every assertion still runs.
NETWORK="$(curl -sS -m "$TIMEOUT" -A "stellarindex-smoke/1" "${API_BASE_URL}/v1/coverage" 2>/dev/null \
  | jq -r '.data.network // "pubnet"' 2>/dev/null)"
[ -n "$NETWORK" ] || NETWORK="pubnet"
case "$NETWORK" in
  testnet|futurenet) NO_PRICES=1; NO_PRICES_WHY="no price sources on $NETWORK" ;;
  *) NO_PRICES=0; NO_PRICES_WHY="" ;;
esac

echo "Smoke-test: ${API_BASE_URL} (network: ${NETWORK})"
echo

echo "  Health"
check "healthz"            "/v1/healthz" -- '.data.status == "ok"'
check "readyz"             "/v1/readyz"
check "version"            "/v1/version" -- '.data.version != null'
check "status"             "/v1/status"  -- '.data.overall'
echo

echo "  Catalogue"
# /v1/assets replaces the removed /v1/coins and /v1/currencies.
check "assets (5)"         "/v1/assets?limit=5" -- '.data | length > 0'
check "asset native"       "/v1/assets/native"  -- '.data.asset_id == "native"'
if [ "$NO_PRICES" -eq 1 ]; then
  skip "assets verified" "$NO_PRICES_WHY"
else
  check "assets verified"    "/v1/assets/verified" -- '.data | length > 0'
fi
check "markets (5)"        "/v1/markets?limit=5"
check "sources"            "/v1/sources"
check "issuers (5)"        "/v1/issuers?limit=5"
check "lending pools"      "/v1/lending/pools"
check "sac wrappers"       "/v1/sac-wrappers"
echo

echo "  Pricing"
if [ "$NO_PRICES" -eq 1 ]; then
  skip "price native/USD" "$NO_PRICES_WHY"
  skip "price tip native/USD" "$NO_PRICES_WHY"
else
  check "price native/USD"   "/v1/price?asset=native&quote=fiat:USD" -- '.data.price | tonumber > 0'
  check "price tip native/USD" "/v1/price/tip?asset=native&quote=fiat:USD"
fi
# /v1/ohlc returns 404 on empty windows (ADR-0018): accept 200|404; 5xx or 400 fail.
expect_status "200|404" "ohlc USDC/XLM"  "/v1/ohlc?base=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&quote=native"
check "history (last 10)"  "/v1/history?base=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&quote=native&limit=10"
check "pairs native/USD"   "/v1/pairs?base=native&quote=fiat:USD"
echo

echo "  VWAP / TWAP"
# Redis MISCONF must surface as 503 cache-unavailable (chaos suite covers that); here 200|404
# since either pair may have no trades in a cold window.
expect_status "200|404" "vwap native/USD" "/v1/vwap?base=native&quote=fiat:USD"
expect_status "200|404" "twap native/USD" "/v1/twap?base=native&quote=fiat:USD"
echo

echo "  Oracle (SEP-40 surface)"
# Same 503 mapping; oracle routes can legitimately 404 with no report in the window.
check "oracle latest"      "/v1/oracle/latest?asset=native"
expect_status "200|404" "oracle lastprice native" "/v1/oracle/lastprice?asset=native"
expect_status "200|404" "oracle prices native"    "/v1/oracle/prices?asset=native"
check "oracle streams"     "/v1/oracle/streams"
expect_status "200|404" "oracle x_last_price"     "/v1/oracle/x_last_price?base=native&quote=fiat:USD"
echo

echo "  Diagnostics"
check "cursors"            "/v1/diagnostics/cursors"
# The route must stay available even when the stale flag is set.
check "diagnostics ingestion" "/v1/diagnostics/ingestion"
check "ledger tip"         "/v1/ledger/tip" -- '.data.latest_ledger | tonumber > 0'
check "network stats"      "/v1/network/stats"
check "incidents"          "/v1/incidents"
check "incidents.atom"     "/v1/incidents.atom"
echo

echo "  Customer surfaces"
# Pin the price_method=vwap invariant: flipping it silently breaks integrators' pricing checks.
check "methodology"        "/v1/methodology" -- '.data.aggregation.price_method == "vwap"'
echo

echo "  Discovery"
# Service-discovery surfaces for crawlers and agents; a 404 here is silent. robots.txt is served
# by Cloudflare, so this only verifies a 200 reaches the client.
check "robots.txt"         "/robots.txt"
# RFC 9116 disclosure metadata, served by handleSecurityTxt.
check "security.txt"       "/.well-known/security.txt"
echo

echo "  Behaviour pins"
# Verify the documented error envelope, so a 4xx weakened into a silent 200 fails the smoke.
expect_status 400 "assets bad limit"     "/v1/assets?limit=999999" \
  -- '.type | endswith("/invalid-limit")'
# Well-formed but nonexistent classic asset_id: passes shape validation and reaches the catalogue
# lookup, giving the documented 404 rather than the 400 for ill-formed input.
expect_status 404 "asset not found"      "/v1/assets/AAAA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" \
  --timeout 30 \
  -- '.type | endswith("/asset-not-found")'
# Exchange venues and on-chain sources are selectable; a data vendor
# (aggregator, FX, oracle) is served only beside other sources.
check "markets cex source" "/v1/markets?source=binance&limit=1"
check "markets on-chain source" "/v1/markets?source=sdex&limit=1"
expect_status 400 "markets data-vendor source" "/v1/markets?source=coingecko&limit=1" \
  -- '.type | endswith("/off-chain-source-filter")'

echo

if [ "$FAILS" -eq 0 ]; then
  printf "%sAll checks passed.%s\n" "$GREEN" "$OFF"
else
  printf "%s%s check(s) FAILED.%s\n" "$RED" "$FAILS" "$OFF"
fi

exit "$FAILS"
