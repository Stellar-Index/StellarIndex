---
title: API performance follow-ups
last_verified: 2026-07-05
status: living doc
---

# API performance follow-ups

Captured during the post-#690 perf-investigation pass. The
route-label fix in #690 stopped masking the slow-request ratio
behind constant `route="unmatched"` denominators; the SLO recording
rules then started reporting real signals.

## Current state on R1 — 2026-05-05 (post-cache rollout)

All three problem endpoints from the original write-up now have
Redis read-through caches in front of them. Cold reads still pay
the underlying DB cost; warm reads are sub-millisecond.

| Endpoint | Cold | Warm | SLA target | Notes |
|----------|-----:|-----:|-----------:|-------|
| `/v1/price` (fiat quote) | ~1 ms | ~1 ms | 200 ms | #692 short-circuit; no DB hit |
| `/v1/oracle/latest` | ~600 ms | ~0.5 ms | 200 ms | Redis 30 s TTL (#696) |
| `/v1/markets` | ~570 ms | ~0.3 ms | — | Redis 60 s TTL (#697) |
| `/v1/assets` | ~635 ms | ~0.4 ms | — | Redis 60 s TTL (#697) |

User impact: the moment any consumer makes more than one
request per minute they see warm reads end-to-end. The smoke
timer firing every 5 min always hits cold cache, which is what
keeps the synthetic-monitoring p99 around the cold-read times —
not a user-experience issue.

## What's already shipped from this investigation

| PR | Effect |
|----|--------|
| **#690** | `obs.HTTPMetrics` + `obs.CaptureRoute`: fixed the route-label-always-`"unmatched"` bug that masked the slow-request ratio. |
| **#691** | `slo.yml` recording rules scope to `/v1/price + /v1/oracle/*` (the SLA target), not the entire API surface. |
| **#692** | `/v1/price` for fiat-quoted pairs short-circuits the `LatestTradesForPair` fallback (a fiat-quoted pair never has on-chain trades). 215 ms → ~1 ms. |
| **#695** | `/v1/oracle/latest` translates `native` → `[native, crypto:XLM]` (and classic credit assets to their `crypto:<TICKER>` form) so Reflector's per-ticker rows actually surface. |
| **#696** | Redis read-through cache for `/v1/oracle/latest`, 30 s TTL — 580 ms → 0.5 ms warm. |
| **#697** | Redis read-through caches for `/v1/assets` + `/v1/markets`, 60 s TTL — both ~600 ms → ~0.3 ms warm. |
| **#689** | `/v1/status` `Cache-Control: public, max-age=10, s-maxage=15` — CDN-friendly polling. |

## What's still pending

### 1. Cold-read latency on the catalogue endpoints

Even with the caches above, the FIRST request after a TTL expires
still pays the full DB cost (~600 ms for /v1/markets, /v1/assets,
/v1/oracle/latest). For high-frequency consumers this is invisible;
for low-frequency consumers (the daily-curl-from-cron use case) it
shows up.

Cold-read fix paths, in order of ambition:

1. **CDN in front of R1.** Cache-Control already emits `s-maxage=N`
   directives. When `api.stellarindex.io` lands behind Cloudflare /
   equivalent, consumers see edge-cache hits for shareable URLs
   regardless of Redis TTL. **Smallest change; operator action.**
2. **Stale-while-revalidate cache.** Serve the warm value
   immediately while refreshing async. The consumer never sees a cold
   read; Redis stays continuously hot. ~50 LOC each on top of the
   existing `cachedOracleReader` / `cachedAssetReader` /
   `cachedMarketsReader` wrappers in `cmd/stellarindex-api/main.go`.
3. **Materialised tables.** `markets_summary` and
   `assets_catalogue` tables maintained by the indexer on every
   trade insert; `Store.DistinctPairs` / `DistinctAssets` read
   directly. Cold reads become O(distinct rows), not O(trades).
   See the original perf-todo for the schema sketches. Multi-PR
   effort; needed only if (1) and (2) prove insufficient at
   sustained consumer volumes.

### 2. `/v1/oracle/latest` cold-read TimescaleDB compressed-chunk indexing

EXPLAIN ANALYZE on R1 showed one specific compressed chunk
(`compress_hyper_11_1126_chunk`) doing a 280 ms `Seq Scan` while
every other chunk does an Index Scan in <0.1 ms. The chunk's
segment-by index appears to be missing or stale. A
`recompress_chunk('compress_hyper_11_1126_chunk', if_not_compressed=>true)`
would rebuild it. **Operator action; not safe to automate
without explicit confirmation** (chunk recompression is a write
operation; if it goes wrong it leaves the chunk in a worse state).

The Redis cache from #696 hides this from user-facing
latency, so this is a "nice to have" rather than urgent.

### 3. Synthetic-monitoring SLO noise

The smoke timer at 5 min fires past the 30 s/60 s cache TTLs and
always sees cold reads. With nothing but synthetic traffic on R1
today, the SLO `slow-request-ratio` recording rule is dominated by
those cold reads — the `stellarindex_slo_latency_burn_*` alerts
keep firing for a real-on-R1 reason that's invisible to actual
consumers (because consumers polling at <1-min cadence see warm
reads).

Three angles, no consensus on which is right:

1. Lengthen smoke cadence past TTL to keep cache warm — but the
   smoke timer's job is regression detection, less frequent
   polling weakens that.
2. Lengthen cache TTLs — but compromises freshness commitments.
3. Tag the smoke probe via `User-Agent` and exclude it from the
   SLO recording rule. Cleanest semantically; the SLO measures
   real consumer experience, not synthetic monitoring.

(3) is the right move; punted to a follow-up when launch traffic
arrives — the noise is a pre-launch artifact and will heal as
real polling fan-out dilutes it.

### 4. `/v1/tx/{hash}` cold lookup ~5–6 s — `tx_hash` has no ordered index

Investigated 2026-06-24 during the SEO audit (the transaction-detail
entity page reads this endpoint).

**Measured on R1:**

| Signal | Value |
|--------|------:|
| `/v1/tx/{hash}` end-to-end (cold, cache-busted) | **5.3–6.3 s** |
| `stellar.transactions` row count | **10,241,480,666** |
| Rows read to resolve one hash | **96,618,934** |
| Server-side query elapsed | **5.41 s** |

**Root cause.** `stellar.transactions` is `ORDER BY (ledger_seq,
tx_index)` — its sort key has nothing to do with `tx_hash`. The only
acceleration on the hash column is a `bloom_filter(0.01)` skip-index
(`idx_tx_hash`, granularity 1). At 10.2 B rows that bloom prunes ~99 %
of granules but the **residual is still ~96.6 M rows** scanned per
lookup. A bloom skip-index fundamentally cannot deliver point-lookup
latency on a high-cardinality random hash at this scale — it prunes,
it does not seek. (`handleTxDetail` → `ExplorerReader.TransactionByHash`
in `internal/storage/clickhouse/explorer_reader.go`. Once the ledger
is known, every downstream query is ledger-scoped and sub-100 ms; the
hash→ledger resolution is the entire cost.)

**This is NOT an SEO blocker.** `/transactions/{hash}` (and the other
long-tail entity shells: `/ledgers/{seq}`, `/accounts/{g}`,
`/contracts/{id}`) ship `robots: { index: false, follow: true }` by the
plan's R2 decision — we deliberately do not index millions of thin
entity pages. Crawlers never fetch `/v1/tx` at scale, so the latency is
a **UX** concern for users who deep-link to a specific transaction, not
a crawl-budget or Core-Web-Vitals problem. The SEO upgrade is complete
without this fix.

**Fix (operator-scale; the standing rule forbids rushing CH backfills
on live R1):** add a hash-ordered lookup table so the resolution is a
binary search, not a scan.

1. **Schema** —
   ```sql
   CREATE TABLE stellar.tx_hash_index
     (tx_hash String, ledger_seq UInt32, tx_index UInt16)
     ENGINE = MergeTree ORDER BY tx_hash;
   CREATE MATERIALIZED VIEW stellar.tx_hash_index_mv TO stellar.tx_hash_index AS
     SELECT tx_hash, ledger_seq, tx_index FROM stellar.transactions;
   ```
   The MV makes every **newly-ingested** tx instantly fast; only three
   narrow columns, so write amplification on the ingest path is small.
2. **Reader** — `TransactionByHash` becomes two steps: `SELECT ledger_seq
   FROM tx_hash_index WHERE tx_hash = ?` (ordered → µs), then the
   existing ledger-scoped summary query `WHERE ledger_seq = ? AND
   tx_hash = ?`. Fall back to today's direct scan when the hash is not
   yet in the index (historical rows, pre-backfill) so there is no
   correctness regression during the backfill.
3. **Historical backfill (the heavy, operator step)** —
   `INSERT INTO stellar.tx_hash_index SELECT tx_hash, ledger_seq,
   tx_index FROM stellar.transactions` over all 10.2 B rows. **Chunk by
   `ledger_seq` range** (e.g. 5 M-ledger windows) and watch the CH log
   partition between chunks — a single unbounded `INSERT … SELECT` over
   10 B rows risks the ClickHouse-log → root-fill → Postgres-crash
   failure mode from the 2026-06-11 incident (logs on the small root
   volume). Run under the root-<2 G watchdog.

Until the backfill runs, lookups of pre-deploy transactions still pay
the scan; new transactions are fast immediately after step 1–2 deploy.
Storage cost of the index is ~3 narrow columns × 10.2 B rows
(`String` hash dominates) — bounded and acceptable.

**Status 2026-07-05 — steps 1–2 SHIPPED as code; step 3 is the
remaining operator action.** The schema (table + MV; ReplacingMergeTree
keyed on `tx_hash` so live-sink retries / `ch-rebuild` re-derives dedup)
is in `deploy/clickhouse/tier1_schema.sql`; the two-step reader with
probe-once availability + scan fallback is
`ExplorerReader.TransactionByHash`; the windowed backfill is
`stellarindex-ops ch-txindex-backfill` (each window prints its `-from`
resume point; re-running a window is idempotent). Full-history
invocation on r1, after applying the schema file (serialize it — don't
run alongside other heavy CH jobs; run under the root-<2 G watchdog):

```sh
clickhouse-client < /path/to/tier1_schema.sql   # CREATE ... IF NOT EXISTS — safe
stellarindex-ops ch-txindex-backfill -ch-addr 127.0.0.1:9300 -window 5000000
# -from defaults to 2, -to 0 = current lake tip; on interrupt re-run
# with the last printed "resume point -from N".
```
