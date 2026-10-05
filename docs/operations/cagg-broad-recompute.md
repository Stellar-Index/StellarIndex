---
title: Operator procedure — broad historical CAGG recompute
last_verified: 2026-10-05
status: current
---

# Broad historical CAGG recompute

One-shot sweep that fills every continuous aggregate over the full preserved raw
range. Run it after the raw window widens; the policies cover everything forward.

## Run when

- After migration 0031 (removed `trades` retention) or 0040 (removed `oracle_updates` retention).
- After an operator raw backfill that lands rows older than a CAGG's oldest bucket
  (the policy backfills forward only).

## Do not run

- 14:00-22:00 UTC (peak SDEX + Soroswap ingest), or beside another heavy job
  (`verify-archive`, bulk-trim, Soroban backfill).
- With the `data` zpool over 85% full.
- For "this CAGG looks stale": that is [cagg-stale](runbooks/cagg-stale.md).

## Procedure

`NULL, NULL` = every chunk older than the CAGG's `end_offset`. One call per grain,
not one transaction; each can take minutes to hours. As the `stellarindex` role
(`psql "$STELLARINDEX_POSTGRES_DSN"`):

```sql
-- trades (migration 0002)
CALL refresh_continuous_aggregate('prices_1m',  NULL, NULL);
CALL refresh_continuous_aggregate('prices_15m', NULL, NULL);
CALL refresh_continuous_aggregate('prices_1h',  NULL, NULL);
CALL refresh_continuous_aggregate('prices_4h',  NULL, NULL);
CALL refresh_continuous_aggregate('prices_1d',  NULL, NULL);
CALL refresh_continuous_aggregate('prices_1w',  NULL, NULL);
CALL refresh_continuous_aggregate('prices_1mo', NULL, NULL);
-- oracle (migration 0034): same seven grains
CALL refresh_continuous_aggregate('oracle_prices_1m',  NULL, NULL);
CALL refresh_continuous_aggregate('oracle_prices_15m', NULL, NULL);
CALL refresh_continuous_aggregate('oracle_prices_1h',  NULL, NULL);
CALL refresh_continuous_aggregate('oracle_prices_4h',  NULL, NULL);
CALL refresh_continuous_aggregate('oracle_prices_1d',  NULL, NULL);
CALL refresh_continuous_aggregate('oracle_prices_1w',  NULL, NULL);
CALL refresh_continuous_aggregate('oracle_prices_1mo', NULL, NULL);
-- pools per source (migration 0036)
CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL);
```

Measured on r1 (2026-05-22, 2.7B trades): full sweep ~4-8 h, `prices_1m` is the long one. Run overnight.

## Watch

```sql
SELECT pid, query_start, state, LEFT(query, 80) AS query
  FROM pg_stat_activity
 WHERE query LIKE '%refresh_continuous_aggregate%' OR query LIKE '%_timescaledb_internal%'
 ORDER BY query_start;
```

```sh
ssh r1 'iostat -x 5 3; df -h /var/lib/postgresql; zpool list data'
```

If pool capacity climbs faster than expected, abort and redo that grain in 1-week slices:

```sql
CALL refresh_continuous_aggregate('prices_1m', '2026-01-01'::timestamptz, '2026-01-08'::timestamptz);
```

## Done when

Each CAGG's oldest bucket is within one chunk width of the raw table's oldest row:

```sql
SELECT (SELECT MIN(bucket) FROM prices_1m)        AS prices_1m_oldest,
       (SELECT MIN(ts)     FROM trades)           AS trades_oldest,
       (SELECT MIN(bucket) FROM oracle_prices_1m) AS oracle_1m_oldest,
       (SELECT MIN(ts)     FROM oracle_updates)   AS oracle_oldest;
```

If one lags, run a targeted `refresh_continuous_aggregate(name, start, end)` for that window.
Design: ADR-0006.
