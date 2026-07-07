-- 0086 up — `protocol_events_24h` worker-maintained rollup.
--
-- Backs the `events_24h` column on GET /v1/protocols and
-- GET /v1/protocols/{name} (internal/api/v1/protocols.go). The live
-- derivation (internal/storage/timescale/protocol_stats.go's
-- `countRecentEventsQuery`) is a UNION ALL count(*) over ~17 served
-- protocol hypertables (trades grouped by source + every projected
-- event table) each filtered to the trailing 24h. The 2026-07-06
-- latency incident measured the whole handler at ~5s cold — this
-- census is the dominant Postgres leg (the ClickHouse event-breakdown
-- scans have their own contract_events_daily fast path).
--
-- Rather than 17 heterogeneous continuous aggregates (the legs span
-- both the `ts` and `ledger_close_time` time columns, so no single
-- CAGG spans them), an aggregator worker
-- (internal/aggregate/protoeventsrollup) runs the census on a slow
-- cadence and upserts one row per source here. The handler then reads
-- a keyed-on-PK table instead of re-counting per request.
--
-- Schema (one row per logical source name):
--   source      — the API protocol-registry source name, e.g. 'sdex',
--                 'blend', 'reflector-dex'. Multi-table sources (blend,
--                 phoenix, comet, soroswap) are summed across their
--                 tables by the worker. The census also yields the
--                 off-chain CEX/FX source rows (binance, kraken, …);
--                 they are a harmless superset — protocol-scoped
--                 callers read only the names they care about.
--   events_24h  — trailing-24h decoded-event count. bigint, not a
--                 monetary value.
--   computed_at — worker run timestamp; the worker prunes rows it did
--                 not re-write this pass (sources that dropped to zero).
--
-- Not a hypertable: keyed on source (bounded cardinality), UPDATE'd in
-- place. Reads return zeros until the aggregator worker's first pass
-- populates it (safe degradation, same posture as change_summary_5m).

BEGIN;

CREATE TABLE protocol_events_24h (
    source      text        PRIMARY KEY,
    events_24h  bigint      NOT NULL,
    computed_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE protocol_events_24h IS
    'Worker-maintained trailing-24h per-source decoded-event count rollup '
    'backing /v1/protocols events_24h (migration 0086, #43). Refreshed by '
    'internal/aggregate/protoeventsrollup in the aggregator binary.';

COMMIT;
