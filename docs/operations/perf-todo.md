---
title: API performance follow-ups
last_verified: 2026-05-05
status: living doc
---

# API performance follow-ups

Captured during the post-#690 perf-investigation pass. The
route-label fix in #690 stopped masking the slow-request ratio
behind constant `route="unmatched"` denominators; the SLO recording
rules then started reporting real signals, and these are the real
signals.

The list excludes `/v1/price` (fixed in #692, now ~1 ms p95 for
fiat-quoted requests) and `/v1/coins` / `/v1/diagnostics/cursors` /
`/v1/sources` / `/v1/history` / `/v1/ohlc` / `/v1/healthz` /
`/v1/readyz` / `/v1/version` / `/v1/status` (all measured under
50 ms p95 on R1 today).

## Current state on R1 — 2026-05-05

| Endpoint | p95 obs | RFP target | In SLO scope? |
|----------|--------:|-----------:|---------------|
| `/v1/price` (fiat quote) | ~1 ms | 200 ms | ✅ in scope |
| `/v1/oracle/latest` | ~430 ms | 200 ms | ✅ in scope |
| `/v1/markets` | ~455 ms | — | excluded by #691 |
| `/v1/assets` | ~475 ms | — | excluded by #691 |

The two SLO-scope alerts (`ratesengine_slo_latency_burn_*`) keep
firing because of `/v1/oracle/latest`. Markets/assets are not
SLO-scope anymore (#691), but they're still slow when CDN-cold —
not a launch blocker because Cache-Control already emits
`s-maxage=60`/`s-maxage=300` directives a CDN will honour.

## 1. `/v1/oracle/latest?asset=native` returns empty in 285 ms

**Root cause.** `oracle_updates` only stores entries keyed by
**Soroban contract IDs** (e.g. `CALI...`, `CAS3...`) — Reflector
publishes per-token observations under the contract address that
holds the price feed. A query for `asset='native'` finds nothing
but pays a ~285 ms hypertable scan to prove it. EXPLAIN ANALYZE on
R1 showed one chunk (`compress_hyper_11_1126_chunk`) doing a
280 ms `Seq Scan` with `Filter: (asset = 'native'::text)` —
Index Scan on every other chunk takes < 0.1 ms.

**Fix paths, in increasing ambition:**

1. **Compressed chunk reindex.** The bad chunk is missing the
   `(source, asset, quote)` segment-by index that the others have.
   `SELECT recompress_chunk('compress_hyper_11_1126_chunk', true)`
   should rebuild it; verify the EXPLAIN goes from `Seq Scan` to
   `Index Scan`. **Smallest change; maybe sufficient on its own.**
2. **Native asset → contract-address translation.** When the
   request asks for `native` (or any classic asset), translate to
   the matching Reflector contract ID (or set of contract IDs)
   before querying. Requires a mapping table; SAC wrappers in
   `[supply.sac_wrappers]` cover most of this already.
3. **Redis cache layer in front of `LatestOracleUpdatesForAsset`.**
   30 s TTL keyed by `(asset, sourceFilter)`. Absorbs 99% of
   polling traffic for popular assets. Non-trivial but reusable
   pattern.

Owner: TBD. Recommend (1) first as a 1-line operator action, then
re-measure before deciding whether (2) or (3) is needed.

## 2. `/v1/markets` 455 ms — full hypertable scan + GROUP BY

**Root cause.** `Store.DistinctPairs` aggregates the trades
hypertable by `(base_asset, quote_asset)` over the 14-day recency
window, then sorts and limits. EXPLAIN ANALYZE on a representative
query showed ~554 ms scanning ~172 K rows that aggregate to
~44 K distinct pairs.

**Fix.** A continuous aggregate / materialised `markets_summary`
table maintained incrementally by the indexer. Reads become O(rows)
where rows = distinct active pairs (~44 K), not O(trades). Schema:

```sql
CREATE TABLE markets_summary (
    base_asset    text NOT NULL,
    quote_asset   text NOT NULL,
    last_trade_at timestamptz NOT NULL,
    trade_count_24h bigint NOT NULL,
    PRIMARY KEY (base_asset, quote_asset)
);
```

Indexer maintains it via `INSERT ... ON CONFLICT DO UPDATE` on each
trade insert. A 5 min cron prunes pairs whose `last_trade_at`
falls outside the recency window.

Owner: TBD. Multi-PR effort: migration → indexer wiring →
`Store.DistinctPairs` switches read path → cutover.

## 3. `/v1/assets` 475 ms — same shape as `/v1/markets`

**Root cause.** `Store.DistinctAssets` does
`UNION` of `DISTINCT base_asset` and `DISTINCT quote_asset` over
the 14-day window. Same hypertable-scan cost as #2 above.

**Fix.** A `assets_catalogue` materialised table populated by the
indexer on every trade insert (UPSERT pattern):

```sql
CREATE TABLE assets_catalogue (
    asset_id       text PRIMARY KEY,
    first_seen_ts  timestamptz NOT NULL,
    last_seen_ts   timestamptz NOT NULL,
    trade_count    bigint NOT NULL DEFAULT 0
);
```

The existing comment at `internal/storage/timescale/assets.go:25`
already calls this out as the planned fix:

> The planned optimisation is a materialised `asset_catalogue`
> populated incrementally by the indexer (future migration; not on
> main today) — that would let us drop the recency bound entirely.

Owner: TBD. Same PR shape as #2.

## 4. CDN in front of R1

Independent of the above — `/v1/markets` and `/v1/assets` already
ship `Cache-Control: public, max-age=N, s-maxage=N` directives
(see `internal/api/v1/middleware/cachecontrol.go`). When the
DNS cutover for `api.ratesengine.net` lands behind a CDN (or
Caddy's `cache_responses` plugin), customers will see sub-50 ms
edge cache hits instead of 400+ ms origin requests. So #2 / #3
above remain real but stop dominating customer-facing latency
once the production setup includes a CDN.

## What's already shipped from this investigation

- **#690** — `obs.HTTPMetrics` + new `obs.CaptureRoute` middleware:
  fixed the route-label-always-`"unmatched"` bug that masked the
  slow-request ratio.
- **#691** — `slo.yml` recording rules now scope to
  `/v1/price + /v1/oracle/*` (the RFP target), not the entire API
  surface.
- **#692** — `/v1/price` for fiat-quoted pairs short-circuits the
  `LatestTradesForPair` fallback (a fiat-quoted pair never has
  on-chain trades). 215 ms → ~1 ms.
