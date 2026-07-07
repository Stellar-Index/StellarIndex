---
title: Runbook — dex-nonstandard-decimals
last_verified: 2026-07-07
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

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_dex_nonstandard_decimals_detected` |
| Severity | P2 |
| Detected by | Prometheus rule in `deploy/monitoring/rules/aggregator.yml` (and `configs/prometheus/rules.r1/aggregator.yml`) |
| Typical MTTR | Minutes to suppress serving; the full normalization is a code change + deploy |
| Impact | A **real, live pair** is being served a price skewed by `10^(7−decimals)`. The `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` values for any pair involving `{asset}` are wrong. |

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

Immediate (stop serving the wrong number):

- [ ] Suppress the affected pair(s) from the served surface. Remove them from
      the aggregator pair set (`[aggregate].pairs`) so no closed-bucket
      `/v1/price` is published, and/or add the asset to the serving denylist
      the API consults, so `/v1/vwap` + `/v1/history` decline rather than
      return a skewed number. A declined price is honest; a wrong price is not.
- [ ] Verification: within one aggregator tick the pair stops publishing; the
      alert stays latched (expected — it is a "this happened" signal, silence
      it via the pair suppression, not by waiting for it to clear).

Durable (the real fix — schedule, do not hot-patch during the incident):

- [ ] Apply the **decimals normalization**: multiply the finalized ratio by
      `10^(dec_base − dec_quote)` at the read layer. See "Root cause analysis"
      for why this was deferred and what a correct fix must cover.

## Root cause analysis

The forward normalization was deliberately **deferred** when this guard
shipped, because a *consistent* fix is not low-risk:

- There are **two** served-price paths that must agree: the materialized
  `prices_*` continuous aggregates (`/v1/history`, `/v1/ohlc`, the closed-1m
  point lookup behind `/v1/price`) and the query-time `aggregate.VWAP` path
  (`/v1/vwap`, `/v1/twap`, the aggregator's Redis-published VWAP).
- Normalizing only the `aggregate.VWAP` path is cheap (it recomputes from raw
  trades each call) but would make it **disagree** with the CAGG path for a
  non-7-dp token — trading one silent error for an inconsistency.
- Normalizing the CAGG path requires **rewriting a decade of materialized
  history** (the `prices_*` aggregates span back to 2015), which is not
  warranted for a latent risk and carries its own operational risk.

A correct fix therefore normalizes **both** read paths together, guarded so a
7-dp/7-dp pair (`10^0 = 1`) is byte-identical to today's behaviour and only
the non-7-dp case is scaled. Track this as the follow-up; this alert is the
forcing function that makes it real work rather than latent.

For the postmortem, gather: the offending contract id, its declared
`decimals()`, the list of affected pairs + their 24h volume, and how long the
skew was live before suppression.

## Known false-positive patterns

- **None expected.** The guard alarms only on a **confirmed** non-7
  `decimals()` (a successful lake read returning a value != 7). A resolution
  error or a token whose metadata isn't yet in the lake is treated as "cannot
  confirm" and never fires. If it fires, a token really does declare non-7
  decimals.
- A token could legitimately declare non-7 decimals and still be low-value /
  low-volume. Check step 2's volume before deciding urgency — but the served
  price is still wrong, so suppression is correct regardless.

## Related

- Implementation: `internal/decimalsguard/guard.go` (the sweep),
  `internal/storage/timescale/soroban_dex_assets.go` (the enumerator),
  `internal/storage/clickhouse/token_decimals_reader.go` (the resolver).
- Metric: `stellarindex_dex_trade_nonstandard_decimals_total` —
  `docs/reference/metrics/README.md`.
- The correctness invariant it protects: ADR-0003 (i128/decimals discipline)
  and the "external-source amount scaling is NOT uniform" note in `CLAUDE.md`.
- Companion serving-sanity guard: `internal/pricingguard` (guards the raw
  closed-bucket `prices_1m` serving path against a different failure mode).

## Changelog

- 2026-07-07 — initial draft alongside the decimals-guard (decoder-correctness
  audit Finding 2).
