#!/usr/bin/env bash
# ch-live-catchup.sh — keep the ClickHouse Tier-1 lake current (ADR-0034 live
# fan-out). Runs ch-backfill over [CH_max+1, indexer_tip], catching CH up to the
# live indexer's confirmed position. Idempotent (ReplacingMergeTree dedups), so
# safe to run on a short timer; each run handles only the new gap. This is the
# operational live fan-out — decoupled from the indexer (CH lags by the timer
# interval), no indexer code change. A future refinement is an in-dispatcher
# dual-sink (write CH inline with Postgres).
set -uo pipefail
set -a; . /etc/default/ratesengine-ops; set +a
OPS=${OPS:-/usr/local/bin/ratesengine-ops-ch}
CFG=${CFG:-/etc/ratesengine.toml}
DSN="$RATESENGINE_POSTGRES_DSN"
PAR=${PAR:-4}

CH_MAX=$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers" 2>/dev/null)
TIP=$(psql "$DSN" -tAc "SELECT max(last_ledger) FROM ingestion_cursors" 2>/dev/null | tr -d '[:space:]')
if [ -z "${CH_MAX:-}" ] || [ -z "${TIP:-}" ]; then
  echo "$(date -u) ch-live-catchup: could not resolve CH_MAX=$CH_MAX / TIP=$TIP" >&2
  exit 1
fi
if [ "$TIP" -le "$CH_MAX" ]; then
  echo "$(date -u) ch-live-catchup: CH current (max=$CH_MAX tip=$TIP)"
  exit 0
fi
FROM=$((CH_MAX + 1))
echo "$(date -u) ch-live-catchup: [$FROM,$TIP] ($((TIP - CH_MAX)) ledgers)"
exec "$OPS" ch-backfill -config "$CFG" -from "$FROM" -to "$TIP" -parallel "$PAR"
