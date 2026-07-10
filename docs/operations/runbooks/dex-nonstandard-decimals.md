---
title: Runbook — dex-nonstandard-decimals
last_verified: 2026-07-10
status: ratified
severity: P2
---

# Runbook — `stellarindex_dex_nonstandard_decimals_detected`

## Why this exists

The served price is `Σ(quote_amount) / Σ(base_amount)` computed on **raw
smallest-unit integers** — both in the `prices_*` continuous aggregates
(`migrations/0002_create_price_aggregates.up.sql`) and in `aggregate.VWAP`
(`internal/aggregate/vwap.go`). The per-asset decimals **cancel in that
ratio only when the base and quote assets share the same decimals scale.**

Every DEX-traded Stellar token to date is 7-decimal (SACs are always 7;
classic credits are uniformly 7; the pure-SEP-41 tokens observed all declare
`decimals = 7`), so the ratio equals the true price and the served value is
correct. The decoders are correct too — they store faithful native-decimal
amounts (ADR-0003); the gap is purely the *absence* of a decimals
normalization on the read side.

The latent risk: the first non-7-decimal SEP-41 token to gain DEX liquidity
(an 18-decimal bridged asset, a 6-dp token, …) makes **every served price
for a pair involving it silently skew by `10^(7−decimals)`**, with no other
alarm. `internal/decimalsguard` sweeps recently-DEX-traded Soroban tokens,
resolves each one's on-chain `decimals()` from the certified lake, and raises
`stellarindex_dex_trade_nonstandard_decimals_total{source,asset}` the moment
one is confirmed `!= 7`. This alert turns that silent landmine into a loud
signal so the mispricing is caught **before** a customer consumes it.

**This stopped being latent on 2026-07-08.** Token
`CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO` declares
`decimals()=9` and traded on aquarius against USDC; the served price was
exactly 100x wrong (`41.32` vs true `~4132`) for 35 trades since
2026-06-22 before the guard confirmed it. That confirmed incident is why
this runbook now has a real stop-serving lever (2026-07-09) instead of
only a detector — see "Mitigation" below.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_dex_nonstandard_decimals_detected` |
| Severity | P2 |
| Detected by | Prometheus rule in `deploy/monitoring/rules/aggregator.yml` (and `configs/prometheus/rules.r1/aggregator.yml`) |
| Typical MTTR | Automatic within ~60s of confirmation (every serving path is normalized live) |
| Impact | A **real, live pair** has a leg with confirmed non-7 `decimals()`. As of 2026-07-10 (second wave), **every serving path is decimals-corrected**: `/v1/vwap`, `/v1/twap`, `/v1/history`, `/v1/ohlc` (BOTH single-bar and `interval=` series modes), `/v1/price` (closed-1m CAGG bucket, batch, and windowed), `/v1/price/tip` (+ SSE), `/v1/chart` (vwap/twap/market-cap price legs), `/v1/markets` / `/v1/pools` / `/v1/pairs` `last_price`, the SEP-40 oracle passthroughs (`/v1/oracle/lastprice`, `/v1/oracle/prices`), and the aggregator's own published VWAP (feeds `/v1/price/stream` + the Redis fallback). Nothing declines anymore — the 422 guard was removed once every path served the corrected value. For any deployment that hasn't run migration 0093 / doesn't wire `NonstandardDecimals`, every path still serves the raw (wrong) ratio with no warning — normalization fails OPEN to 7dp. |

## Symptoms

- The alert names a `source` (DEX connector) and `asset` (Soroban C-strkey
  contract id).
- The aggregator log (component `decimals-guard`) has an ERROR line with the
  exact `decimals` value and `price_skew_decades` (`|7 − decimals|`).
- The counter latches: once a token is detected it stays firing for the
  process lifetime (dedup is per `source`+`asset`).

## Quick diagnosis (≤ 5 min)

```sh
# 1. What decimals does the offending token declare? (the guard already
#    logged it; confirm from the lake directly)
journalctl -u stellarindex-aggregator | grep decimals-guard | tail

# 2. Which pairs are affected — every served pair with {asset} on either leg:
psql "$PG" -c "SELECT DISTINCT base_asset, quote_asset, source
               FROM trades
               WHERE (base_asset = '<asset>' OR quote_asset = '<asset>')
                 AND ts >= now() - interval '24 hours';"

# 3. How wrong is the served price? For a pair with a 7-dp counter-leg:
#    served = true_price * 10^(7 - decimals) when {asset} is the BASE leg,
#    served = true_price * 10^(decimals - 7) when {asset} is the QUOTE leg.
#    e.g. an 18-dp base token → served is 10^-11 of the true price.
```

If the token genuinely declares `decimals != 7`, this is a **true positive** —
the served price for those pairs is wrong. Proceed to mitigation.

## Mitigation (≤ 15 min)

Immediate (stop serving the wrong number) — **now mostly automatic**:

- [ ] **Nothing to do by default — including for a token that is already
      DORMANT.** The moment `internal/decimalsguard` confirms an offender,
      it upserts `nonstandard_decimals_assets` (migration 0093) via
      `UpsertNonstandardDecimalsAsset`. This now happens two ways: the
      periodic freshness sweep (20-minute trailing window, catches a token
      that is STILL trading) and — since 2026-07-09 — a one-time startup
      `Guard.Backfill` pass that scans a much longer historical window
      (default 90 days, `[decimals_guard].backfill_window_days`) exactly
      once when the aggregator process starts, so a token that traded and
      then went quiet is confirmed and upserted without waiting for it to
      trade again. The API's `NonstandardDecimalsCache` (`internal/api/v1`)
      refreshes that table every `NonstandardDecimalsRefreshInterval` (60s)
      and, from then on, every price-shaped serving path NORMALIZES the
      served value by `10^(dec_base − dec_quote)` for any pair with
      `{asset}` on either leg (`aggregate.ResolveDecimals` +
      `aggregate.AdjustPrice`). The next request/tick after the cache
      refresh serves the CORRECT price — no decline, no re-derive, no
      backfill.
- [ ] Verification: confirm the row exists —
      `psql "$PG" -c "SELECT * FROM nonstandard_decimals_assets WHERE asset = '<asset>';"`
      — and confirm the API is serving the CORRECTED price —
      `curl -s "$API_BASE_URL/v1/price?asset=<asset>&quote=fiat:USD" | jq .data.price`
      should show the decimals-corrected value (raw × 10^(decimals−7) for
      a flagged base leg) within ~60s of the row appearing. If the API
      still serves the raw skewed value: check whether this deployment's
      `cmd/stellarindex-api` binary predates the fix, or whether
      `NonstandardDecimals` failed to wire (see "Known false-positive
      patterns" — a nil cache fails OPEN, i.e. serves raw). If the row
      is missing for a token you can see traded in `trades` history, check
      the aggregator log for `decimals-guard: startup backfill complete` —
      its `scanned_pairs`/`confirmed_offenders` counts confirm the pass
      actually ran (only runs once per process start, not on every restart
      loop iteration).
- [ ] The alert stays latched (expected — it is a "this happened" signal.
      The old live decline counter
      `stellarindex_price_serve_declined_nonstandard_decimals_total{asset}`
      is HISTORICAL since 2026-07-10 — the decline path it counted was
      replaced by normalization, so it is permanently zero).
- [ ] **Manual hand-seed — now a fallback, not the primary path.** Only
      needed when: the token's last trade is OLDER than
      `backfill_window_days` (default 90d, so the startup pass never scans
      back far enough), the aggregator hasn't restarted since this fix
      shipped, or ClickHouse was unreachable at the aggregator's last
      startup (the whole guard — sweep AND backfill — is best-effort and
      silently disabled without a lake connection). In any of those cases:
      `INSERT INTO nonstandard_decimals_assets (asset, decimals, source,
      confirmed_at) VALUES ('<asset>', <decimals>, '<source>', now());` —
      the API cache picks it up on its next refresh, no restart needed.
      This is the same row shape the guard writes; the table doesn't
      distinguish operator-inserted rows from guard-confirmed ones. A
      faster alternative to a manual INSERT: restart the aggregator — the
      startup backfill pass runs unconditionally on every boot and will
      self-seed any historically-traded offender within its window.

Durable (**shipped in full** — query-time half 2026-07-10 (v0.12.0), CAGG-
reading tail later the same day):

- [x] Applied the **decimals normalization** as a READ-TIME scalar multiply:
      `aggregate.AdjustPrice(raw, baseDecimals, quoteDecimals)` scales a
      finished price ratio (VWAP/TWAP/OHLC open/high/low/close) by
      `10^(dec_base − dec_quote)`, resolved per-request from
      `nonstandard_decimals_assets` via `aggregate.ResolveDecimals` (nil
      lookup or an unflagged asset ⇒ `StandardDecimals=7` ⇒ exact no-op).
      Covers (first wave, v0.12.0): `internal/aggregate/orchestrator` (the
      SERVED VWAP behind `/v1/price/stream` and `/v1/price`'s Redis
      fallback — this had NO decline guard, so it was the actual live
      leak), `/v1/vwap`, `/v1/twap`, `/v1/history` (the per-row `price`
      field), `/v1/ohlc` single-bar mode, and `/v1/price/tip` (+ its SSE
      sibling — also previously unguarded).
- [x] **CAGG-reading tail (shipped 2026-07-10, same AdjustPrice recipe):**
      `/v1/price`'s closed-1m-bucket path (incl. the batch endpoint's
      direct read and the last-trade fallback inside `LatestPrice`),
      `/v1/ohlc?interval=` series mode (incl. the fiat-combined series),
      `/v1/chart` (`HistoryPointsInRange` / `TWAPPointsInRange` + the
      market-cap price leg), the `last_price` field on `/v1/markets` /
      `/v1/pools` / `/v1/pairs` (`pools_per_source_1h`'s
      `last(quote_amount/base_amount, ts)`, migration 0036), and the
      SEP-40 oracle passthroughs (`/v1/oracle/lastprice`,
      `/v1/oracle/prices` — found unnormalized during this work, same raw
      `prices_1m` read). Each applies `AdjustPrice` to the CAGG-read value
      at serve time; the CAGGs themselves are untouched (see "Root cause
      analysis" for why that is exact). Volume fields are deliberately NOT
      scaled: OHLC `v_base`/`v_quote` are raw smallest-unit sums in each
      asset's OWN declared decimals (the wire contract — matching
      `/v1/history`, which serves raw amounts + `base_decimals`/
      `quote_decimals` metadata), and chart `v_usd` is already
      USD-anchored (`Σ usd_volume`). With no path left declining,
      `declineIfNonstandardDecimals` was deleted and its 422 responses
      removed from the OpenAPI spec (v1.6.0);
      `stellarindex_price_serve_declined_nonstandard_decimals_total` is
      retained but permanently zero. Once `{asset}` is confirmed, the next
      request/tick to ANY endpoint serves the corrected price immediately
      — no re-derive, no backfill, no cache-clear needed.
- [ ] **Residual (known, accepted):** consumers reading `prices_*` /
      `trades` directly via SQL (bypassing the Go API layer) still see the
      raw unnormalized ratio — apply `10^(dec_base − dec_quote)` by hand
      (see "Quick diagnosis"). The `/v1/markets`/`/v1/pools` listing
      caches (Redis + in-process, ~2 min TTL) can serve a pre-confirmation
      raw `last_price` for up to one TTL after a new confirmation, because
      normalization is applied per-request AFTER the cache read — bounded
      by the same ~60s+TTL window as the old decline guard. And
      `pools_per_source_1h`'s XLM-fallback USD-volume estimate hardcodes
      `/1e7` for the XLM leg — XLM is genuinely 7dp, so this is correct,
      but a non-XLM fallback leg would need the same treatment if ever
      added.

## Root cause analysis

**Why a read-time scalar multiply is exact, and why it doesn't need to
touch the CAGGs at all:** every trade contributing to one VWAP/TWAP/OHLC
call — or one CAGG bucket row — shares the SAME `(base_asset, quote_asset)`
pair, so the decimals-correction factor `K = 10^(dec_base − dec_quote)` is a
single constant for that whole call/row. VWAP, TWAP, and OHLC's open/high/
low/close are all linear or monotonic functions of the per-trade ratio
`quote_i/base_i`, so multiplying the FINISHED value by `K` is exactly
equivalent to normalizing every trade before summing — without touching the
summation, the CAGG SQL, or any stored row. This is the mechanism this fix
ships: `aggregate.AdjustPrice` applied immediately after
`aggregate.VWAP`/`TWAP`/`ComputeOHLC` returns (query-time paths), and inside
`internal/aggregate/orchestrator.refreshPairWindow` right after the
window's VWAP computes (the served/cached path).

**Why the CAGG-backed paths could be fixed WITHOUT touching the CAGGs
(the follow-up shipped 2026-07-10):** the exact same
`AdjustPrice(raw_cagg_value, dec_base, dec_quote)` recipe applies to a
value READ OUT of `prices_1m`/`prices_1h`/etc. — the same "one constant
factor per pair" argument holds whether the raw ratio came from a live
trade scan or a pre-materialized bucket. This is materially CHEAPER than
the "rewrite a decade of materialized history" plan an earlier draft of
this runbook assumed necessary — zero migration, zero CAGG redefinition,
zero re-materialization risk. The one audit it required (`/v1/price`'s
several fallback branches) resolved as: the primary closed-1m read + its
last-trade fallback are RAW and get `AdjustPrice`
(`normalizeRawPriceSnapshot` in `internal/api/v1/price.go`); the
`priceFallback` chain does NOT (its Redis-VWAP branch is already
normalized upstream by the orchestrator's `computeNormalizedVWAP`, and
its peg / fiat-cross-rate branches never carry a flagged Soroban leg) —
double-applying there would re-skew an already-correct value. The same
per-call-site treatment landed in `ohlc_series.go`
(`adjustOHLCSeriesBars`), `chart.go` (`adjustHistoryPointPrices`),
`markets.go`/`pairs.go` (`adjustListingPrice`), and `oracle_sep40.go`.

**Why NOT insert-time decimals stamping (a `trades.base_decimals` /
`quote_decimals` column, populated at insert):** considered and rejected in
favor of the read-time design above. A trade's declared decimals is an
immutable per-ASSET fact (a token's `decimals()` never changes post-deploy),
not a per-trade-time-varying one — every row in a `(base_asset,
quote_asset)` CAGG bucket, no matter when it was ingested, shares the exact
same correction factor. That means there is no "late-confirmed decimals
corrupts an already-mixed bucket" risk to defend against, and therefore no
backfill/re-derive/catch-up SQL is needed: the moment
`nonstandard_decimals_assets` gets a confirmed row, the VERY NEXT read
(query-time call or CAGG read) is
correct, for both new AND already-stored historical trades. Insert-time
stamping would have added a migration + a new not-old-binary-trivial write
path for zero additional correctness, so it was not pursued. The one
residual gap this leaves: a consumer that reads `prices_*` / `trades`
directly via SQL (bypassing the API layer entirely — an ad-hoc analyst
query, a dashboard hitting Postgres directly) still sees the RAW,
unnormalized ratio; the fix above only corrects values that leave the
process through the Go API/orchestrator layer. Apply the same
`10^(dec_base − dec_quote)` factor by hand for any such query — see the
"Quick diagnosis" section above for the exact skew-direction formula.

For the postmortem, gather: the offending contract id, its declared
`decimals()`, the list of affected pairs + their 24h volume, and how long the
skew was live before normalization/suppression.

## Known false-positive patterns

- **None expected.** The guard alarms only on a **confirmed** non-7
  `decimals()` (a successful lake read returning a value != 7). A resolution
  error or a token whose metadata isn't yet in the lake is treated as "cannot
  confirm" and never fires. If it fires, a token really does declare non-7
  decimals.
- A token could legitimately declare non-7 decimals and still be low-value /
  low-volume. Check step 2's volume before deciding urgency — but the served
  price is wrong until the row lands, so confirmation is correct regardless.
- **The API-side normalization fails OPEN, never closed.** A nil
  `NonstandardDecimalsCache` (not wired in `cmd/stellarindex-api/main.go`), a
  cold cache (never refreshed yet), or a refresh error (Postgres blip —
  tracked by `stellarindex_nonstandard_decimals_cache_refresh_failures_total`,
  which retains the last-good snapshot rather than clearing it) all mean
  every asset resolves to the 7dp default and prices serve RAW rather than
  corrected. This is deliberate — availability wins over the guard for
  infra errors — but it means a confirmed offender can still serve its raw
  (skewed) value briefly during an API-process restart before the initial
  cache refresh completes, or indefinitely on a deployment that predates
  migration 0093 / hasn't wired `NonstandardDecimals`.

## Related

- Detection: `internal/decimalsguard/guard.go` (the periodic sweep,
  `Guard.Sweep`, AND the one-time startup self-seed pass, `Guard.Backfill`
  — both share the same classify+report path), `internal/storage/timescale/soroban_dex_assets.go`
  (the shared time-bounded enumerator both call with different windows),
  `internal/storage/clickhouse/token_decimals_reader.go` (the resolver).
  Backfill's lookback window is config-surfaced:
  `internal/config.DecimalsGuardConfig.BackfillWindowDays` (`[decimals_guard]`
  in `configs/example.toml`), default 90 days
  (`decimalsguard.DefaultBackfillWindow`).
- Enforcement (historical — the 422 decline, 2026-07-09 → 2026-07-10):
  `internal/api/v1/nonstandard_decimals_guard.go` was DELETED on
  2026-07-10 when the last declining paths gained normalization; the
  cache it consulted remains the live source of truth:
  `internal/api/v1/nonstandard_decimals_cache.go` (`NonstandardDecimalsCache`),
  `internal/storage/timescale/nonstandard_decimals_assets.go`
  (`UpsertNonstandardDecimalsAsset` / `LoadNonstandardDecimalsAssets`),
  `migrations/0093_create_nonstandard_decimals_assets.up.sql`.
- Normalization (correcting, 2026-07-10): `internal/aggregate/decimals.go`
  (`AdjustPrice` / `ResolveDecimals` / `DecimalsLookup` — the shared
  primitive), `internal/aggregate/orchestrator.go` (`Config.DecimalsLookup`,
  applied in `refreshPairWindow`), `cmd/stellarindex-aggregator/decimals_cache.go`
  (the aggregator binary's own mirror of `nonstandard_decimals_assets`),
  `internal/api/v1/vwap.go` / `twap.go` / `ohlc.go` (single-bar mode) /
  `history.go` / `price_tip.go` (each applies `AdjustPrice` after computing
  from raw trades), and — CAGG-reading tail, 2026-07-10 —
  `internal/api/v1/price.go` (`normalizeRawPriceSnapshot`: closed-1m
  bucket + batch + last-trade fallback), `ohlc_series.go`
  (`adjustOHLCSeriesBars`), `chart.go` (`adjustHistoryPointPrices`),
  `markets.go` / `pairs.go` (`adjustListingPrice` on `last_price`),
  `oracle_sep40.go` (SEP-40 passthroughs).
- Metrics: `stellarindex_dex_trade_nonstandard_decimals_total` (detection),
  `stellarindex_price_serve_declined_nonstandard_decimals_total` (live
  enforcement impact), `stellarindex_nonstandard_decimals_cache_refresh_failures_total`
  (cache infra health) — `docs/reference/metrics/README.md`.
- The correctness invariant it protects: ADR-0003 (i128/decimals discipline)
  and the "external-source amount scaling is NOT uniform" note in `CLAUDE.md`.
- Companion serving-sanity guard: `internal/pricingguard` (guards the raw
  closed-bucket `prices_1m` serving path against a different failure mode —
  gross single-bucket manipulation, not a per-asset decimals mismatch).

## Changelog

- 2026-07-10 (second wave) — **the deferred CAGG-reading tail shipped**,
  completing normalization for every serving path: `/v1/price`'s
  closed-1m-bucket read (+ batch + last-trade fallback + windowed via the
  already-normalized orchestrator cache), `/v1/ohlc?interval=` series
  mode (incl. fiat-combined), `/v1/chart` (vwap/twap/market-cap price
  legs), `last_price` on `/v1/markets` / `/v1/pools` / `/v1/pairs`, and
  the SEP-40 oracle passthroughs (found unnormalized during this work).
  Volume fields deliberately untouched: OHLC `v_base`/`v_quote` are raw
  smallest-unit sums per the wire contract (matching `/v1/history`'s
  raw amounts + decimals metadata); chart `v_usd` is already
  USD-anchored. With nothing left declining, the 422 guard
  (`declineIfNonstandardDecimals`) was deleted, its 422 responses removed
  from the OpenAPI spec (v1.6.0), and
  `stellarindex_price_serve_declined_nonstandard_decimals_total` became
  permanently zero (retained one release for dashboard continuity).
- 2026-07-10 — **forward normalization shipped** for every query-time
  serving path: `internal/aggregate.AdjustPrice` (a per-pair
  `10^(dec_base−dec_quote)` scalar applied to the finished VWAP/TWAP/OHLC
  ratio, resolved from `nonstandard_decimals_assets`) is now applied by
  the aggregator's orchestrator (the served/Redis-cached VWAP —
  previously the actual unguarded live leak, since it feeds
  `/v1/price/stream` and had no decline in front of it), `/v1/vwap`,
  `/v1/twap`, `/v1/history`, `/v1/ohlc` single-bar mode, and
  `/v1/price/tip` (+ SSE — also previously unguarded, found during this
  work). `declineIfNonstandardDecimals` was removed from those five
  endpoints' call sites and now only gates the two remaining CAGG-backed
  paths, `/v1/price` (closed-1m bucket) and `/v1/ohlc?interval=` (series
  mode) — see "Root cause analysis" for why those were deliberately left
  as a documented follow-up rather than forced into this change, and why
  no migration / trades-table schema change / historical backfill was
  needed for any of this (a read-time scalar is exact and self-corrects
  the instant a token is confirmed).
- 2026-07-09 — closed the DORMANT-token self-seed gap: the guard's periodic
  sweep only ever enumerated a 20-minute trailing window, so `CC2RB…` (see
  the previous entry) traded from 2026-06-22 but was never automatically
  confirmed — it wasn't trading at the moment the guard shipped and never
  traded inside a 20-minute window again, so it stayed unseeded until an
  operator hand-inserted the row. Added `Guard.Backfill`: a one-time pass,
  run once at aggregator startup before the periodic loop begins, that
  scans a much longer historical window (default 90 days, config-surfaced
  via `[decimals_guard].backfill_window_days`) through the SAME
  classify+report path the periodic sweep uses, and logs a
  `decimals-guard: startup backfill complete` summary
  (`scanned_pairs`/`confirmed_offenders`). The manual `INSERT` above is now
  a fallback for the residual case (a token dormant longer than the
  backfill window, or a process that hasn't restarted since this shipped),
  not the primary remediation path.
- 2026-07-09 — added the READ-TIME enforcement half after a confirmed
  production incident (token `CC2RB…`, 100x-wrong price, 35 trades since
  2026-06-22): `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` now
  DECLINE (422) rather than serve a price for a pair with a confirmed
  non-7-decimals leg. Migration 0093 (`nonstandard_decimals_assets`),
  `internal/decimalsguard` now upserts on confirmation,
  `internal/api/v1.NonstandardDecimalsCache` mirrors it API-side. Self-clearing
  once the durable normalization ships and the row is removed.
- 2026-07-07 — initial draft alongside the decimals-guard (decoder-correctness
  audit Finding 2).
