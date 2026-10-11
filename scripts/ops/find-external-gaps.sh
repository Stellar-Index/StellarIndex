#!/usr/bin/env bash
# find-external-gaps.sh — report DAY-level holes in each external (CEX/oracle)
# source's trade history.
#
# `find-data-gaps` does not cover this: it is LEDGER-scoped, right for on-chain
# sources but unable to see an external one (a Kraken fill has no ledger), so a
# CEX source could go dark unnoticed. One did, for 26 months; it left
# prices_1m/1h/1d for crypto:XLM/fiat:USD empty and ~1.7M SDEX trades stored
# with a NULL usd_volume.
#
# A reported gap is not automatically a defect: a venue can genuinely not trade
# a pair for a while (kraken has no XLM/USD between 2017-09 and 2018-01; its own
# API returns zero for those days). This reports; a human decides which holes
# are ours.
#
# Usage:
#   find-external-gaps.sh                     # every external source
#   find-external-gaps.sh kraken bitstamp     # named sources
#   MIN_GAP_DAYS=3 find-external-gaps.sh      # only runs >= 3 days (default 2)
# Env: STELLARINDEX_POSTGRES_DSN (required). Exit 0 no gaps, 1 gaps found,
# 2 DSN unset.
set -euo pipefail

MIN_GAP_DAYS="${MIN_GAP_DAYS:-2}"
DSN="${STELLARINDEX_POSTGRES_DSN:-}"
if [ -z "$DSN" ]; then
    echo "find-external-gaps: STELLARINDEX_POSTGRES_DSN is unset — source /etc/default/stellarindex" >&2
    exit 2
fi

SOURCES=("$@")
if [ "${#SOURCES[@]}" -eq 0 ]; then
    # External tier only; on-chain sources belong to find-data-gaps (ledger contiguity).
    SOURCES=(kraken coinbase bitstamp binance)
fi

rc=0
for src in "${SOURCES[@]}"; do
    # One scan per source. Zero-row days INSIDE the source's active range are
    # holes; outside it they are "not started yet" / "ended".
    out="$(psql "$DSN" -tAF'|' <<SQL
WITH bounds AS (
    SELECT min(ts)::date AS lo, max(ts)::date AS hi
    FROM trades WHERE source = '${src}'
), present AS (
    SELECT DISTINCT ts::date AS d
    FROM trades WHERE source = '${src}'
), all_days AS (
    SELECT generate_series(lo, hi, interval '1 day')::date AS d FROM bounds
), missing AS (
    SELECT a.d FROM all_days a LEFT JOIN present p USING (d) WHERE p.d IS NULL
), runs AS (
    SELECT d, d - (row_number() OVER (ORDER BY d))::int AS grp FROM missing
)
SELECT min(d), max(d), count(*)
FROM runs GROUP BY grp HAVING count(*) >= ${MIN_GAP_DAYS} ORDER BY 1;
SQL
)"
    if [ -z "$out" ]; then
        echo "find-external-gaps: ${src}: no gap >= ${MIN_GAP_DAYS} day(s)"
        continue
    fi
    n="$(printf '%s\n' "$out" | grep -c '|' || true)"
    echo "find-external-gaps: ${src}: ${n} gap(s) >= ${MIN_GAP_DAYS} day(s)"
    printf '%s\n' "$out" | while IFS='|' read -r a b c; do
        [ -n "$a" ] && printf '  %s .. %s  (%s day(s))\n' "$a" "$b" "$c"
    done
    rc=1
done
exit "$rc"
