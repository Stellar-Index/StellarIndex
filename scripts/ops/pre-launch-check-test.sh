#!/usr/bin/env bash
# pre-launch-check-test.sh — fixture tests for the Healthchecks.io and
# USD volume pricing sections of scripts/ops/pre-launch-check.sh.
#
# wire-paging.md's acceptance test is "pre-launch-check.sh reports 0
# failures", so every heartbeat URL that runbook has the operator create
# must FAIL the check when it is missing. ss/systemctl/curl/journalctl
# are stubbed on PATH so the run is hermetic on any box; only the
# HEALTHCHECKS_URL_* result lines are asserted on.
#
# Run: bash scripts/ops/pre-launch-check-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/pre-launch-check.sh"
[[ -r "$SCRIPT" ]] || { echo "pre-launch-check-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin"
for tool in ss systemctl curl journalctl; do
  printf '#!/bin/sh\nexit 1\n' >"$TMP/bin/$tool"
  chmod +x "$TMP/bin/$tool"
done

URLS=(HEALTHCHECKS_URL_INDEXER HEALTHCHECKS_URL_AGGREGATOR HEALTHCHECKS_URL_API
      HEALTHCHECKS_URL_SMOKE HEALTHCHECKS_URL_SLA_PROBE)

# run_hc <env-file> — prints the script's HEALTHCHECKS_URL_* result lines.
run_hc() {
  PATH="$TMP/bin:$PATH" \
    STELLARINDEX_TOML="$TMP/absent.toml" \
    HEALTHCHECKS_ENV_FILE="$1" \
    ALERTMANAGER_ENV_FILE="$TMP/absent-am" \
    bash "$SCRIPT" 2>/dev/null | grep 'HEALTHCHECKS_URL_'
}

# ── 1. every runbook URL set, unquoted (install.sh's shape) ────────────
: >"$TMP/hc-all"
for v in "${URLS[@]}"; do
  printf '%s=https://hc-ping.com/%s\n' "$v" "$v" >>"$TMP/hc-all"
done
out="$(run_hc "$TMP/hc-all")"
if grep -q 'FAIL' <<<"$out"; then
  bad "all five URLs set → no HC failure (got: $out)"
else
  ok "all five URLs set → no HC failure"
fi
for v in "${URLS[@]}"; do
  if grep -qE "ok .*$v" <<<"$out"; then ok "$v reported set"; else bad "$v not reported set (got: $out)"; fi
done

# ── 2. each URL empty in turn → exactly that URL FAILs ─────────────────
for missing in "${URLS[@]}"; do
  sed "s|^$missing=.*|$missing=|" "$TMP/hc-all" >"$TMP/hc-missing"
  out="$(run_hc "$TMP/hc-missing")"
  if grep -qE "FAIL .*$missing" <<<"$out" && [[ "$(grep -c 'FAIL' <<<"$out")" -eq 1 ]]; then
    ok "$missing empty → FAIL counted"
  else
    bad "$missing empty → expected exactly its FAIL (got: $out)"
  fi
done

# ── 3. single-quoted values (pre-launch-hardening.md's shape) → set ───
sed -E "s|=(https://.*)$|='\1'|" "$TMP/hc-all" >"$TMP/hc-quoted"
out="$(run_hc "$TMP/hc-quoted")"
if grep -q 'FAIL' <<<"$out"; then
  bad "single-quoted URLs → no HC failure (got: $out)"
else
  ok "single-quoted URLs → no HC failure"
fi

# ── 4. an empty quoted value is still unset ───────────────────────────
sed "s|^HEALTHCHECKS_URL_SLA_PROBE=.*|HEALTHCHECKS_URL_SLA_PROBE=''|" "$TMP/hc-all" >"$TMP/hc-emptyquoted"
out="$(run_hc "$TMP/hc-emptyquoted")"
if grep -qE 'FAIL .*HEALTHCHECKS_URL_SLA_PROBE' <<<"$out"; then
  ok "HEALTHCHECKS_URL_SLA_PROBE='' → FAIL counted"
else
  bad "HEALTHCHECKS_URL_SLA_PROBE='' → expected FAIL (got: $out)"
fi

# ── 5. USD volume pricing bars gate the cutover ───────────────────────
# A curl stub serves /v1/coverage and the Prometheus ALERTS query from
# fixture files; an unset fixture behaves like an unreachable endpoint.
mkdir -p "$TMP/usdbin"
cp "$TMP/bin/"{ss,systemctl,journalctl} "$TMP/usdbin/"
cat >"$TMP/usdbin/curl" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    */v1/coverage)    [ -n "${STUB_COV:-}" ]  && exec cat "$STUB_COV";  exit 7 ;;
    */api/v1/query)   [ -n "${STUB_PROM:-}" ] && exec cat "$STUB_PROM"; exit 7 ;;
  esac
done
exit 1
STUB
chmod +x "$TMP/usdbin/curl"

# run_usd <coverage-fixture|""> <prom-fixture|""> — prints the section's result lines.
run_usd() {
  local all
  all="$(PATH="$TMP/usdbin:$PATH" STUB_COV="$1" STUB_PROM="$2" \
    STELLARINDEX_TOML="$TMP/absent.toml" HEALTHCHECKS_ENV_FILE="$TMP/hc-all" \
    ALERTMANAGER_ENV_FILE="$TMP/absent-am" \
    bash "$SCRIPT" 2>/dev/null)"
  awk '/^  USD volume pricing$/{on=1; next} on && /^$/{exit} on' <<<"$all"
}
src() { printf '{"source":"%s","class":"%s","trades":%s,"priced":%s,"unpriced":0,"unroutable":0,"priced_ratio":"%s","bar":"%s"%s}' "$@"; }
cov() { printf '{"data":{"usd_volume_pricing":{"lower_bound":false,"sources":[%s]}}}' "$1"; }
green_ext="$(src binance external 10 10 1.000000 0.999 ',"meets_bar":true')"
sdex="$(src sdex onchain 100 90 0.900000 0.995 '')"
cov "$green_ext,$sdex" >"$TMP/cov-green"
cov "$green_ext,$(src kraken external 1000 990 0.990000 0.999 ',"meets_bar":false')" >"$TMP/cov-kraken"
printf '{"data":{"usd_volume_pricing":null}}' >"$TMP/cov-null"
printf '{"data":{"usd_volume_pricing":{"sources":null}}}' >"$TMP/cov-nosrc"
printf '{"data":{"usd_volume_pricing":{"lower_bound":false}}}' >"$TMP/cov-nosrc2"
printf '{"data":{"network":"pubnet"}}' >"$TMP/cov-absent"
printf '{"status":"success","data":{"resultType":"vector","result":[]}}' >"$TMP/prom-quiet"
printf '{"status":"success","data":{"resultType":"vector","result":[{"metric":{"alertname":"stellarindex_onchain_usd_volume_coverage_low"},"value":[0,"1"]}]}}' >"$TMP/prom-sdex"

# expect_usd <name> <out> <want-fails> <want-warns> [grep-pattern-that-must-match]
expect_usd() {
  local nf nw
  nf="$(grep -c 'FAIL' <<<"$2")"; nw="$(grep -c 'WARN' <<<"$2")"
  if [[ "$nf" -eq "$3" && "$nw" -eq "$4" ]] && { [[ -z "${5:-}" ]] || grep -qE "$5" <<<"$2"; }; then
    ok "$1"
  else
    bad "$1 (want $3 FAIL/$4 WARN${5:+ matching $5}; got: $2)"
  fi
}
expect_usd "external bars met, no alerts → passes (on-chain has no meets_bar)" \
  "$(run_usd "$TMP/cov-green" "$TMP/prom-quiet")" 0 0 'ok .*external venues meet'
expect_usd "external venue below bar → FAIL names it" \
  "$(run_usd "$TMP/cov-kraken" "$TMP/prom-quiet")" 1 0 'FAIL .*below bar: kraken .*0\.990000'
expect_usd "axis null (no snapshot yet) → FAIL, fail-closed" \
  "$(run_usd "$TMP/cov-null" "$TMP/prom-quiet")" 1 0 'FAIL +usd_volume_pricing not computed.*first refresh'
expect_usd "sources null → FAIL, never a pass" \
  "$(run_usd "$TMP/cov-nosrc" "$TMP/prom-quiet")" 1 0 'FAIL +usd_volume_pricing.sources'
expect_usd "sources missing → FAIL, never a pass" \
  "$(run_usd "$TMP/cov-nosrc2" "$TMP/prom-quiet")" 1 0 'FAIL +usd_volume_pricing.sources'
expect_usd "axis absent from /v1/coverage → FAIL" \
  "$(run_usd "$TMP/cov-absent" "$TMP/prom-quiet")" 1 0 'FAIL +usd_volume_pricing'
expect_usd "/v1/coverage unreachable → FAIL" \
  "$(run_usd "" "$TMP/prom-quiet")" 1 0 'FAIL +/v1/coverage'
expect_usd "on-chain coverage alert firing → FAIL names it" \
  "$(run_usd "$TMP/cov-green" "$TMP/prom-sdex")" 1 0 'FAIL .*stellarindex_onchain_usd_volume_coverage_low'
expect_usd "Prometheus unreachable → FAIL, never a silent pass" \
  "$(run_usd "$TMP/cov-green" "")" 1 0 'cannot query Prometheus'

echo
echo "pre-launch-check-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
