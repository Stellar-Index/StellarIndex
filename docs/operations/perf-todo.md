---
title: API performance follow-ups
last_verified: 2026-08-20
status: living doc
---

# API performance follow-ups

> **⚠️ PARTIALLY SUPERSEDED — reconciled 2026-08-20 (supersession first
> flagged 2026-08-14).** The "Current state on R1" table below is a
> **2026-05-05 historical snapshot**, not present-day behaviour — see the
> present-day note beneath it. Several original "still pending" items have
> since shipped; the rest are genuinely open. Do NOT build fresh SWR
> wrappers or re-investigate the tx-index — check the shipped architecture
> first. Evidence for every "SHIPPED" claim is in `CHANGELOG.md`.
>
> **Shipped since this doc was written:**
> - **Item 1.2 (stale-while-revalidate) — SHIPPED.** Catalogue endpoints
>   serve stale-while-revalidate off a continuously-hot Redis snapshot,
>   refreshed by the API's 5-minute prewarm loop under the class-fair
>   detached-refresh gate. The v0.28.1–v0.33.2 refresh-gate campaign
>   generalised this shape across API + explorer. (`stale-while-revalidate`,
>   `prewarm`, `explorer_swr_refresh`.)
> - **Item 3 (SLO synthetic-monitoring noise) — SHIPPED.** Synthetic probes
>   (`stellarindex-smoke/`, `stellarindex-probe/`, `stellarindex-prewarm/`)
>   are dropped from the latency histogram and `slo.yml` carries a
>   min-traffic floor, so cold synthetic reads no longer dominate the burn
>   alerts. (`stellarindex-smoke`; `deploy/monitoring/rules/slo.yml`.)
> - **Item 4 (ordered `tx_hash_index` + backfill) — SHIPPED.**
>   (`tx_hash_index`.)
>
> **Still open:** item 1.1 (Cloudflare orange-cloud proxy in front of
> `api.` — operator action; still grey-cloud today), item 2 (Timescale
> `/v1/oracle/latest` compressed-chunk recompress — operator action), and
> item 5 (`/v1/accounts/{g}/movements` extreme-address timeout, BACKLOG
> #72). **Item 1.3 (bespoke `markets_summary` / `assets_catalogue`
> materialised tables) was NEVER BUILT** — it was always a contingency and
> item 1.2 removed the need; there is no such table in the schema or code,
> so do not treat it as shipped *or* as pending.

Captured during the post-#690 perf-investigation pass. The
route-label fix in #690 stopped masking the slow-request ratio
behind constant `route="unmatched"` denominators; the SLO recording
rules then started reporting real signals.

## Historical snapshot — R1 on 2026-05-05 (post-cache rollout)

> This section is a **2026-05-05 snapshot**, retained for history. It does
> not describe present-day serving behaviour — see the present-day note
> after the table.

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

> **Present-day R1 (2026-08-20).** The table above no longer reflects
> serving behaviour. The catalogue endpoints (`/v1/oracle/latest`,
> `/v1/markets`, `/v1/assets`, and the wider explorer surface) now serve
> **stale-while-revalidate** off a continuously-hot Redis snapshot kept
> warm by the API's 5-minute prewarm loop under the class-fair
> detached-refresh gate — consumers no longer meet a request-path cold
> read, even on the first request after a TTL boundary (item 1.2). The
> synthetic-monitoring noise described above is also resolved (item 3):
> smoke/probe/prewarm requests are excluded from the SLO latency
> histogram. See `CHANGELOG.md` (`stale-while-revalidate`, `prewarm`).

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

1. **CDN in front of R1. — STILL OPEN (operator action).** Cache-Control
   already emits `s-maxage=N` directives. When `api.stellarindex.io` lands
   behind Cloudflare / equivalent, consumers see edge-cache hits for
   shareable URLs regardless of Redis TTL. **Smallest change; operator
   action.** Today `api.` is DNS-live but **grey-cloud** (direct to the R1
   origin); the orange-cloud proxy is documented in `cdn-setup.md` and
   tracked as launch-todo P2-1 ④ — recommended, not yet enabled.
2. **Stale-while-revalidate cache. — SHIPPED (v0.28.1–v0.33.2).** Serve
   the warm value immediately while refreshing async. The consumer never
   sees a cold read; Redis stays continuously hot. This is now the
   established serving shape across the API and explorer (detached
   single-flight refresh under the class-fair refresh gate, tied to the
   5-minute prewarm loop); do NOT add new bespoke SWR wrappers — reuse it.
   See `CHANGELOG.md` (`stale-while-revalidate`, `prewarm`,
   `explorer_swr_refresh`).
3. **Materialised tables. — NOT BUILT (superseded by option 2).** This was
   always a contingency: "needed only if (1) and (2) prove insufficient."
   Option (2) — stale-while-revalidate + prewarm — shipped and removed the
   request-path cold read, so the bespoke `markets_summary` /
   `assets_catalogue` tables were **never built**: there is no such table
   in the ClickHouse/Timescale schema or in code (a repo-wide search finds
   `markets_summary` / `assets_catalogue` only in this doc). Do not treat
   this as shipped. Revisit only if SWR + prewarm prove insufficient at
   sustained consumer volumes. For reference, the original design:
   `markets_summary` and `assets_catalogue` tables maintained by the
   indexer on every trade insert; `Store.DistinctPairs` / `DistinctAssets`
   read directly, making cold reads O(distinct rows), not O(trades). See
   the original perf-todo history for the schema sketches. Multi-PR effort.

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

### 3. Synthetic-monitoring SLO noise — SHIPPED

> **SHIPPED.** The recommended angle (3) below landed: the HTTP metrics
> middleware drops requests whose User-Agent identifies a synthetic probe
> (`stellarindex-smoke/`, `stellarindex-probe/`, later also
> `stellarindex-prewarm/`) from the latency histogram, so the SLO measures
> real consumer experience. A second, belt-and-suspenders layer lives in
> `slo.yml` (both the multi-host rules and the R1 overlay): a min-traffic
> floor (~5 req/s ≈ 2× the synthetic baseline) keeps the burn alerts from
> firing on synthetic-only traffic. See `CHANGELOG.md` (`stellarindex-smoke`)
> and `deploy/monitoring/rules/slo.yml`. The analysis below is retained for
> the original reasoning.

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

(3) was the right move and is what shipped — synthetic-UA exclusion from
the SLO histogram — with the `slo.yml` min-traffic floor as a second
layer. Retained here for the original reasoning.

### 4. `/v1/tx/{hash}` cold lookup ~5–6 s — `tx_hash` has no ordered index — SHIPPED

> **SHIPPED.** The ordered `stellar.tx_hash_index` (hash-keyed point
> lookup) plus its `tx_hash_index_mv` and the `ch-txindex-backfill` ops
> subcommand for history now back this endpoint, guarded by the hourly
> `tx_hash_index_parity` assertion. Do NOT re-investigate the seq-scan —
> see `CHANGELOG.md` (`tx_hash_index`). Retained below for the original
> measurements.

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

### 5. `/v1/accounts/{g}/movements` extreme-address timeout (BACKLOG #72)

- **Symptom:** > 20 s for extreme-volume addresses (airdrop-sink accounts, e.g.
  264M movements). Ordinary addresses serve fast.
- **Root cause:** `stellar.account_movements` is `PARTITION BY intDiv(ledger,
  1000000)` (~473 partitions). An address's movements scatter across the ~140
  partitions it was ever active in, and the reverse-keyset read
  (`WHERE address = ? ORDER BY ledger DESC … LIMIT ?`) must open + merge all of
  them. `address` already LEADS the table's `ORDER BY`, so the within-partition
  read is already optimal — the cost is the cross-partition fan-out, which
  ledger-range partitioning makes unavoidable.
- **Projection evaluated + REJECTED (2026-07-16):** a ClickHouse `PROJECTION`
  is always co-partitioned with its parent, so it cannot reduce the fan-out.
  An `(address, ledger, …)` projection merely DUPLICATES this table's existing
  sort order at ~2× storage for ~zero durable benefit. (This corrects the
  ROADMAP's "structural fix: PROJECTION on (address, ledger)" note, which
  predated the fact that the base table is already address-leading.)
- **Partly transient:** Phase-0's concurrent genesis-extension writes into old
  partitions inflate part counts; ReplacingMergeTree + background merges
  collapse this post-Phase-0. **RE-EVALUATE after Phase 0 completes + merges
  settle** before doing anything structural — it may self-heal below the
  timeout.
- **Real structural fix (only if it persists):** a `PARTITION BY` change or an
  address-keyed secondary structure (e.g. a second table/MV partitioned by
  `cityHash64(address) % N`) — a full 6.76B-row rebuild, heavy, out of scope
  until there's a measured need. **Interim:** accept-and-monitor; if it becomes
  user-facing, add a per-query timeout + a "history too large to page
  interactively" response for the handful of extreme addresses.
