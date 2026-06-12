#!/usr/bin/env bash
# ch-supply-refresh.sh — recompute per-token supply into stellar.token_supply
# from the live (dual-sink-fed) ClickHouse lake, keeping it current for the
# explorer. Full re-derive (Σmint−Σburn−Σclawback per contract over all history)
# — simple + always-correct-as-of-run; idempotent (ReplacingMergeTree by
# contract_id replaces the prior row). ~16 min / ~570 M flows; runs nice'd on a
# timer (supply is slow-changing, so a few-hours cadence is ample).
#
# NOTE: runs -final=false (fast/light) — carries the sample-partition (25/45/62)
# dup caveat from docs/architecture/clickhouse-supply-from-ch.md §7 until those
# partitions are deduped. A near-real-time path (incremental delta, or a CH
# materialized view once the extractor decodes the amount into a column) is the
# documented follow-up.
set -uo pipefail
set -a; . /etc/default/stellarindex-ops; set +a
OPS=${OPS:-/usr/local/bin/stellarindex-ops-ch}
CFG=${CFG:-/etc/stellarindex.toml}
echo "$(date -u) ch-supply-refresh: recomputing stellar.token_supply"
exec "$OPS" ch-supply -config "$CFG" -from 2 -to 999999999 -final=false -write -top 0
