---
title: Runbook — dex
last_verified: 2026-10-06
status: living
severity: P2
---

# Runbook — dex alerts

## At a glance

DEX decimals and trade-unit alerts, plus the Soroswap registry bootstrap.

DEX data-quality alerts: [`dex-nonstandard-decimals`](#dex-nonstandard-decimals) and [`stellarindex_dex_trade_unit_ratio_detected`](#stellarindex_dex_trade_unit_ratio_detected), plus a [bootstrap](#bootstrap-seed-the-soroswap-pair-registry) procedure.

## dex-nonstandard-decimals

Covers `stellarindex_nonstandard_decimals_correction_failing` (ticket, P2). Rule: `deploy/monitoring/rules/aggregator.yml` (twin `configs/prometheus/rules.r1/aggregator.yml`). The per-token detection counter `stellarindex_dex_trade_nonstandard_decimals_total{source,asset}` is a dashboard signal only; detection is handled automatically.

Served price is `sum(quote_amount) / sum(base_amount)` on raw integers, so it is only correct when both legs share a decimals scale. A token with `decimals() != 7` skews every pair it is on by `10^(7-decimals)` unless corrected. `internal/decimalsguard` confirms `decimals()` from the lake and upserts `nonstandard_decimals_assets` (migration 0093); every price-shaped serving path then normalises through `aggregate.AdjustPrice`. Normalisation fails OPEN to 7dp: with a nil, cold or erroring cache, prices serve raw.

### Check

The alert has three arms; the first two name no token. Which arm:

```sh
curl -s http://localhost:9465/metrics | grep -E 'nonstandard_decimals_cache_refresh_failures_total|price_serve_declined_nonstandard_decimals_total'
journalctl -u stellarindex-aggregator | grep decimals-guard | tail
```

- `cache_refresh_failures_total` rising: the API's 60 s refresh of `nonstandard_decimals_assets` fails against Postgres (or migration 0093 is missing). The cache keeps its last-good snapshot.
- `price_serve_declined_nonstandard_decimals_total` rising: should be permanently zero; a serving path regressed to declining. Look for a recent deploy touching `internal/api/v1` price/vwap/twap/ohlc/history/chart/markets/pairs/oracle code and confirm `declineIfNonstandardDecimals` was not reintroduced.
- `stellarindex_nonstandard_decimals_lockstep_mismatch_total{site,asset}` rising: the lake `decimals()` and the projection disagree for `asset`. Read the ERROR `decimals-guard: nonstandard_decimals_assets row DISAGREES with the lake` for `persisted_decimals` vs `lake_decimals`.
  - `site="guard_reconcile"`: drift repaired on the 15 min tick (a hand-seed that contradicted the instance is the usual cause).
  - `site="asset_detail"`, `asset_listing`, `rwa_contract`, `lending_reserve`: the endpoint withheld `market_cap_usd` / `fdv_usd` (`market_cap_decimals_mismatch: true`) rather than publish a number off by a power of ten. A sustained count with no `guard_reconcile` repair means the guard is not converging; check the sweep and whether the token is outside the 90-day backfill window.

For a flagged token, find affected pairs and confirm the row and the served price:

```sh
psql "$PG" -c "SELECT DISTINCT base_asset, quote_asset, source
               FROM trades
               WHERE (base_asset = '<asset>' OR quote_asset = '<asset>')
                 AND ts >= now() - interval '24 hours';"
psql "$PG" -c "SELECT * FROM nonstandard_decimals_assets WHERE asset = '<asset>';"
curl -s "$API_BASE_URL/v1/price?asset=<asset>&quote=fiat:USD" | jq .data.price
```

Raw skew: served = true * 10^(7-decimals) when the asset is the BASE leg, true * 10^(decimals-7) when QUOTE. The price should show the corrected value within ~60 s of the row appearing.

### Likely causes

- API cannot reach Postgres, or migration 0093 missing on this deployment.
- Serving-path regression (decline reintroduced).
- Guard not running: ClickHouse unreachable at aggregator start disables the whole guard (sweep and backfill).
- Lake and projection drifted (hand-seeded row, or token outside the backfill window).

### Fix

1. Cache failures: restore API-to-Postgres connectivity; refresh resumes from the last-good snapshot. Missing table: run the migrator.
2. Decline regression: roll back or fix the call site; the counter stops rising.
3. New token not corrected yet: restart the aggregator (the startup backfill scans 90 days, `decimalsguard.DefaultBackfillWindow`), or hand-seed (needed when the last trade is older than 90 days or ClickHouse was down at start; the API cache picks it up within 60 s, no restart):

```sql
INSERT INTO nonstandard_decimals_assets (asset, decimals, source, confirmed_at)
VALUES ('<asset>', <decimals>, '<source>', now());
```

4. Lockstep: fix the wrong row (hand-seed or guard bug); a `guard_reconcile` hit already repaired itself.
5. Residual: consumers reading `prices_*` / `trades` via SQL see raw ratios (apply `10^(dec_base - dec_quote)` by hand); `/v1/markets` and `/v1/pools` caches may show a raw `last_price` for up to one TTL (~2 min) after confirmation.

Escalate: a sustained cache-refresh failure or any decline increment. Related: [`aggregator.md#stellarindex_nonstandard_decimals_correction_failing`](aggregator.md#stellarindex_nonstandard_decimals_correction_failing), [`aggregator.md#stellarindex_decimals_guard_sweep_stale`](aggregator.md#stellarindex_decimals_guard_sweep_stale).

## stellarindex_dex_trade_unit_ratio_detected

P2 (ticket). Rule: `deploy/monitoring/rules/ingestion.yml` (twin `configs/prometheus/rules.r1/ingestion.yml`). One source's landed on-chain trades with `base_amount == quote_amount` exceed 25 in 30 min AND 50% of that source's inserts (`stellarindex_dex_trade_unit_ratio_total{source}` vs `stellarindex_trade_inserts_total`): the decoder field-mapping fingerprint. Prices for pairs on that source skew toward 1:1; cross-check [divergence.md](divergence.md). The counter is emitted from `InsertTrade` / `BatchInsertTrades` in `internal/storage/timescale/trades.go`.

### Check

```sh
# Systemic (every trade 1:1) or a few genuine equal-value fills?
psql "$PG" -c \
  "SELECT base_asset, quote_asset, base_amount, quote_amount, ts
     FROM trades
    WHERE source = '<source>'
      AND ts > now() - interval '30 minutes'
    ORDER BY ts DESC LIMIT 50;"
psql "$PG" -c \
  "SELECT count(*) FILTER (WHERE base_amount = quote_amount) AS unit_ratio,
          count(*) AS total
     FROM trades
    WHERE source = '<source>' AND ledger <> 0
      AND ts > now() - interval '1 hour';"
# If systemic, compare the decoder's amount mapping with the contract's
# ACTUAL event definition (contracts upgrade in place: use the deployed WASM).
grep -n "BaseAmount\|QuoteAmount" internal/sources/<source>/decode.go
```

### Likely causes

- Decoder field-mapping swap or collapse in `decode.go` (true positive).
- Genuine equal-value fills (wrapper redemptions, stablecoin crosses): should stay under 50% of flow; a source crossing it across multiple unrelated pairs is the real signal.
- Dust amounts rounding to equal integers (check trade size).

### Fix

1. Systemic: fix `decode.go` against the contract's event schema.
2. If the mispricing is live, pull the source from `[aggregate].pairs` / the serving denylist so a declined price ships instead of a wrong one.
3. After the fix ships, purge corrupted rows for the range and re-derive. Always pass `-sources`: without it the run re-derives every event-based source. Non-projected sources:

```sh
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml -from <ledger> -to <ledger> -sources sdex -sdex -write
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml -from <ledger> -to <ledger> -sources <name> -contract-calls -write
```

   Projected sources:

```sh
stellarindex-ops projector-replay -config /etc/stellarindex.toml -source <name> -from <ledger> -write
```

   Which side a source is on: [the replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule). Do not merge fixed and corrupted rows for the same range.
4. Verify `increase(stellarindex_dex_trade_unit_ratio_total{source="<source>"}[30m])` falls under the thresholds and stays there.

Escalate: for the postmortem, record the window, the decoder diff and `SELECT count(*) FROM trades WHERE source = '<source>' AND base_amount = quote_amount AND ts BETWEEN <window>`.

## Bootstrap: seed the Soroswap pair registry

Run once on a new deployment, before the first Soroswap swap is decoded. The indexer refuses to start without the registry (`soroswap_pairs`); live `new_pair` events keep it current afterwards. The command walks the factory's `all_pairs()` through stellar-rpc, inserts pairs not yet registered, and fails if a registered pair disagrees. It is a dry run until `-write`.

```sh
stellarindex-ops seed-soroswap-pairs -config /etc/stellarindex.toml -write
```

## Related

- [Alerts catalogue](../alerts-catalog.md)
