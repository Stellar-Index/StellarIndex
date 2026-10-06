---
title: API performance follow-ups
last_verified: 2026-10-05
status: living doc
---

# API performance follow-ups

Open API performance items, and the operator notes for the shipped
`tx_hash_index` (§4, cited from code).

**Shipped — do not rebuild:**
- **Stale-while-revalidate** on the catalogue endpoints (`/v1/oracle/latest`,
  `/v1/markets`, `/v1/assets`, explorer surface): served off a continuously-hot
  Redis snapshot, refreshed by the API's 5-minute prewarm loop under the
  class-fair detached-refresh gate. Consumers never meet a request-path cold
  read. Do NOT add bespoke SWR wrappers — reuse it.
- **Synthetic traffic excluded from the SLO:** the HTTP metrics middleware
  drops `stellarindex-smoke/`, `stellarindex-probe/` and `stellarindex-prewarm/`
  User-Agents from the latency histogram, and `deploy/monitoring/rules/slo.yml`
  carries a min-traffic floor (~5 req/s ≈ 2× the synthetic baseline).
- **`tx_hash_index`** for `/v1/tx/{hash}` (§4).

## What's still pending

### 1. Cold-read latency on the catalogue endpoints

1. **CDN in front of R1 — OPEN (operator action).** Responses already emit
   `s-maxage=N`. `api.stellarindex.io` is DNS-live but **grey-cloud** (direct
   to the R1 origin); the orange-cloud proxy is in
   [`cdn-setup.md`](cdn-setup.md), a pre-launch hardening step ([`pre-launch-hardening.md`](pre-launch-hardening.md)).
2. **Stale-while-revalidate — SHIPPED** (above).
3. **Materialised catalogue tables — NOT BUILT; deferred.** Revisit only if
   SWR + prewarm prove insufficient at sustained consumer volume. Design:
   `markets_summary` and `assets_catalogue` tables maintained by the indexer on
   every trade insert, read directly by `Store.DistinctPairs` /
   `DistinctAssets`, making a cold read O(distinct rows) instead of
   O(trades). Multi-PR effort. No such table exists in schema or code.

### 2. `/v1/oracle/latest` compressed-chunk Seq Scan (TimescaleDB)

EXPLAIN ANALYZE on R1 showed `compress_hyper_11_1126_chunk` doing a 280 ms
`Seq Scan` while every other chunk did an Index Scan in <0.1 ms (segment-by
index missing or stale). Fix:
`recompress_chunk('compress_hyper_11_1126_chunk', if_not_compressed=>true)`.
**Operator action; do not automate** — recompression is a write and a failure
leaves the chunk worse. Low priority: the cache hides it from users.

### 4. `/v1/tx/{hash}` — `tx_hash_index` (SHIPPED)

`stellar.transactions` is `ORDER BY (ledger_seq, tx_index)`; its only hash
acceleration is a `bloom_filter(0.01)` skip-index (`idx_tx_hash`), which at
10.2 B rows still scanned ~96.6 M rows per lookup (5.3–6.3 s cold). The fix
is a hash-ordered lookup:

- **Schema** (`deploy/clickhouse/tier1_schema.sql`): `stellar.tx_hash_index`
  (`tx_hash, ledger_seq, tx_index`; ReplacingMergeTree keyed on `tx_hash`, so
  live-sink retries and `ch-rebuild` re-derives dedupe) fed by
  `stellar.tx_hash_index_mv`. New transactions are indexed on ingest.
- **Reader**: `ExplorerReader.TransactionByHash` resolves hash → ledger from
  the index (probe-once availability), then runs the ledger-scoped query; it
  falls back to the direct scan when the hash is not indexed, so there is no
  correctness gap mid-backfill.
- **Guard**: hourly `tx_hash_index_parity` assertion.
- **Historical backfill** (operator): `stellarindex-ops ch-txindex-backfill`,
  windowed by `ledger_seq`; each window prints its `-from` resume point and
  re-running a window is idempotent.

Operator cautions for the full-history run on r1: serialize it (no other heavy
CH job alongside), run under the root-<2 G watchdog, and watch the CH log
partition between windows — one unbounded `INSERT … SELECT` over 10 B rows
risks the CH-log → root-fill → Postgres-crash failure mode.

```sh
clickhouse-client < /path/to/tier1_schema.sql   # CREATE ... IF NOT EXISTS — safe
stellarindex-ops ch-txindex-backfill -ch-addr 127.0.0.1:9300 -full -window 5000000 -write
# -from defaults to 2, -to 0 = current lake tip; on interrupt re-run
# with the last printed "resume point -from N".
```

### 5. `/v1/accounts/{g}/movements` extreme-address timeout (BACKLOG #72)

- **Symptom:** > 20 s for extreme-volume addresses (airdrop sinks, e.g. 264M
  movements). Ordinary addresses are fast.
- **Cause:** `stellar.account_movements` is `PARTITION BY intDiv(ledger,
  1000000)`; an address spans ~140 partitions and the reverse-keyset read
  (`WHERE address = ? ORDER BY ledger DESC … LIMIT ?`) opens and merges all of
  them. `address` already leads `ORDER BY`, so the cost is cross-partition
  fan-out.
- **Rejected:** a `PROJECTION` on `(address, ledger)` — projections are
  co-partitioned with the parent, so it cannot cut the fan-out and only
  doubles storage.
- **Next:** re-measure after Phase 0's genesis-extension writes stop and
  merges settle; it may self-heal. If it persists: a `PARTITION BY` change or
  an address-keyed secondary table/MV partitioned by `cityHash64(address) % N`
  (full 6.76B-row rebuild). Interim: accept and monitor; if user-facing, add a
  per-query timeout and a "history too large to page interactively" response.

Shipped: §3 SLO burn alerts `stellarindex_slo_latency_burn_*` and slow-request ratio — see git history for before/after timings
