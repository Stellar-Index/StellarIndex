#!/usr/bin/env bash
# reconcile-supply-vs-horizon.sh — compare EVERY tracked classic asset's
# served total_supply against public Horizon's component sum.
#
# Why this exists: the confidence campaign's B3 track verified Algorithm 2
# against the TRUSTLINE sum alone, which is exact — and therefore never
# exercised the other three components. That blind spot hid a 13.2%
# understatement on AQUA (unseeded claimable balances, 2026-07-27). This
# script closes it by reconciling against the FULL component sum:
#
#     trustlines(authorized) + claimable + liquidity pools + contracts(SAC)
#
# Horizon is a legitimate oracle here for the same reason reconcile-balances
# uses it: a one-off, read-only external verifier is exactly the case
# ADR-0001's Horizon ban does not cover (that ban scopes to production
# ingest). It shares no cause of error with our pipeline, which is the whole
# point of an oracle.
#
# Read-only. Exit code = number of assets outside tolerance (capped 255), so
# cron/Healthchecks can consume it — the same convention as r1-smoke.sh.
#
# Usage:
#   scripts/ops/reconcile-supply-vs-horizon.sh [-t TOLERANCE_PCT] [-a API_BASE]
set -euo pipefail

TOLERANCE_PCT="${TOLERANCE_PCT:-1.0}"
API_BASE="${API_BASE_URL:-https://api.stellarindex.io}"
HORIZON="${HORIZON_URL:-https://horizon.stellar.org}"

while getopts "t:a:h" opt; do
  case "$opt" in
    t) TOLERANCE_PCT="$OPTARG" ;;
    a) API_BASE="$OPTARG" ;;
    h) sed -n '2,25p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done

# The tracked classic-asset set (Algorithm 2). Sourced from the live
# trustline_observations distinct asset_key set on r1, 2026-07-27. Keep in
# sync with [supply] watched_classic_assets.
ASSETS=(
  "AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
  "BLND:GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
  "EURC:GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
  "KALE:GBDVX4VELCDSQ54KQJYTNHXAHFLBCA77ZY2USQBM4CSHTTV7DME7KALE"
  "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
  "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
  "VELO:GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
  "yXLM:GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
)

printf '# supply reconciliation vs Horizon — %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '# tolerance %s%%   api=%s\n\n' "$TOLERANCE_PCT" "$API_BASE"
printf '%-6s %14s %14s %14s %14s %16s %16s %9s  %s\n' \
  ASSET TRUSTLINES CLAIMABLE POOLS CONTRACTS HORIZON_TOTAL OURS DELTA_PCT VERDICT

fails=0
for a in "${ASSETS[@]}"; do
  code="${a%%:*}"; issuer="${a##*:}"

  h=$(curl -s --max-time 30 "${HORIZON}/assets?asset_code=${code}&asset_issuer=${issuer}" || echo '{}')
  read -r tl cb lp ct < <(echo "$h" | jq -r '
    ._embedded.records[0] // {} |
    [ (.balances.authorized // "0"),
      (.claimable_balances_amount // "0"),
      (.liquidity_pools_amount // "0"),
      (.contracts_amount // "0") ] | @tsv' | tr '\t' ' ')

  ours=$(curl -s --max-time 30 "${API_BASE}/v1/assets/${code}-${issuer}" \
         | jq -r '(.data.total_supply // .total_supply) // empty')

  if [ -z "${ours:-}" ] || [ -z "${tl:-}" ]; then
    printf '%-6s %14s %14s %14s %14s %16s %16s %9s  %s\n' \
      "$code" "${tl:--}" "${cb:--}" "${lp:--}" "${ct:--}" "-" "${ours:--}" "-" "SKIP(no-data)"
    continue
  fi

  # Horizon reports decimal units; ours is stroops. Compare in decimal via
  # awk (bash has no float). 1 unit = 10^7 stroops.
  read -r htot omine delta verdict < <(awk -v tl="$tl" -v cb="$cb" -v lp="$lp" -v ct="$ct" \
      -v ours="$ours" -v tol="$TOLERANCE_PCT" 'BEGIN{
    htot = tl + cb + lp + ct;
    omine = ours / 10000000.0;
    if (htot == 0) { print htot, omine, "inf", "SKIP(zero)"; exit }
    d = (omine - htot) / htot * 100.0;
    ad = (d < 0) ? -d : d;
    printf "%.4f %.4f %+.4f %s\n", htot, omine, d, (ad > tol ? "FAIL" : "PASS");
  }')

  [ "$verdict" = "FAIL" ] && fails=$((fails + 1))
  printf '%-6s %14.2f %14.2f %14.2f %14.2f %16.2f %16.2f %8s%%  %s\n' \
    "$code" "$tl" "$cb" "$lp" "$ct" "$htot" "$omine" "$delta" "$verdict"
  sleep 0.3
done

echo
echo "assets=${#ASSETS[@]} outside_tolerance=${fails}"
[ "$fails" -gt 255 ] && fails=255
exit "$fails"
