---
title: Runbook — dex-trade-unit-ratio
last_verified: 2026-07-09
status: ratified
severity: P2
---

# Runbook — `stellarindex_dex_trade_unit_ratio_detected`

## Why this exists

On 2026-07-07 every Phoenix DEX trade — 237,000 rows — was found with
`base_amount == quote_amount`: a decoder field-mapping bug had collapsed
every trade's implied price to an exact 1:1 ratio. It went unnoticed for
months because ADR-0033 completeness checks verify **presence** (a row
landed for every event) not **plausibility** (does the row's number make
sense). A 100%-complete decoder can still be economically wrong.

`stellarindex_dex_trade_unit_ratio_total{source}` is the cheap sentinel
that closes that gap: it counts every landed, on-chain trade whose
`base_amount` exactly equals its `quote_amount` (both nonzero). It's
emitted from `internal/storage/timescale`'s `InsertTrade` +
`BatchInsertTrades` — the one seam every trade write funnels through
exactly once, regardless of whether it arrived via the dispatcher's live
batch path, the projector's per-event sink, or a `stellarindex-ops
ch-rebuild` / backfill re-derive. This alert fires when one source
produces a **sustained stream** of these — an occasional equal-value
cross-asset fill is normal, so the threshold tolerates that noise.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_dex_trade_unit_ratio_detected` |
| Severity | P2 (ticket, not page — data-quality signal, not an outage) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/ingestion.yml` (and `configs/prometheus/rules.r1/ingestion.yml`) |
| Typical MTTR | Minutes to confirm + suppress; the decoder fix itself is a code change + redeploy + historical re-derive |
| Impact | Every price derived from the affected source's trades is wrong — served `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` for pairs involving that source are silently skewed toward 1:1. |

## Symptoms

- The alert names a `source` (the on-chain DEX connector — soroswap /
  aquarius / phoenix / comet / sdex).
- `sum by (source) (increase(stellarindex_dex_trade_unit_ratio_total[30m]))`
  is above 25 and climbing for that source.
- Prices for pairs traded predominantly on that source look "too close to
  1" relative to other sources or reference prices (cross-check
  `price-divergence.md`).

## Quick diagnosis (≤ 5 min)

```sh
# 1. Check the source's recent trades ratio distribution — is this
#    systemic (every trade 1:1) or a handful of genuine equal-value
#    fills?
psql "$PG" -c \
  "SELECT base_asset, quote_asset, base_amount, quote_amount, ts
     FROM trades
    WHERE source = '<source>'
      AND ts > now() - interval '30 minutes'
    ORDER BY ts DESC LIMIT 50;"

# 2. Quantify: what fraction of the source's recent on-chain trades are
#    unit-ratio?
psql "$PG" -c \
  "SELECT count(*) FILTER (WHERE base_amount = quote_amount) AS unit_ratio,
          count(*) AS total
     FROM trades
    WHERE source = '<source>' AND ledger <> 0
      AND ts > now() - interval '1 hour';"

# 3. If step 2 shows this is systemic (most/all trades unit-ratio),
#    check the decoder's amount field mapping against the contract's
#    ACTUAL event definition — the 2026-07-07 root cause was exactly
#    this class of bug (a field-mapping swap/collapse in decode.go).
#    Soroban contracts upgrade in place (CLAUDE.md) — verify against
#    the currently-deployed WASM, not stale docs.
grep -n "BaseAmount\|QuoteAmount" internal/sources/<source>/decode.go
```

If step 2 shows the source's recent trades are overwhelmingly
unit-ratio (not a handful), this is a **true positive** — proceed to
mitigation. A low, steady background rate on a source with genuinely
frequent equal-value fills is the known false-positive pattern below.

## Mitigation (≤ 15 min)

- [ ] Step 1 — confirm systemic vs occasional via the diagnosis above.
- [ ] Step 2 — if systemic: compare `decode.go`'s field mapping against
      the contract's actual event schema and fix it.
- [ ] Step 3 — if the mispricing is live and customer-facing while the
      fix is prepared, suppress the source from serving (same
      suppression path as `dex-nonstandard-decimals.md`: pull it from
      `[aggregate].pairs` / the serving denylist so a declined price
      ships instead of a wrong one).
- [ ] Step 4 — after the decoder fix ships, purge the corrupted rows for
      the affected range and re-derive the source's history from the
      ClickHouse lake (ADR-0034): `stellarindex-ops ch-rebuild -source
      <name>` for non-projected sources, or `stellarindex-ops
      projector-replay -source <name> -from <ledger>` for projected
      ones. Don't merge fixed and corrupted rows for the same range.
- [ ] Verification:
      `increase(stellarindex_dex_trade_unit_ratio_total{source="<source>"}[30m])`
      drops back under the 25 threshold and stays there.

## Root cause analysis

For the postmortem, gather: the affected source + how long the bug was
live, the exact decoder diff that introduced it, the number of
corrupted rows (`SELECT count(*) FROM trades WHERE source = '<source>'
AND base_amount = quote_amount AND ts BETWEEN <window>`), and whether
any downstream aggregate (VWAP, continuous aggregates) needs
recomputation after the re-derive.

## Known false-positive patterns

- **Genuine equal-value cross-asset fills.** A source that legitimately
  trades near-1:1 pairs (e.g. two USD-pegged stablecoins) can produce a
  real `base_amount == quote_amount` trade occasionally. The
  25-per-30-minute threshold is sized to absorb this; a source crossing
  it repeatedly across *multiple, unrelated* pairs — not one recurring
  stablecoin pair — is the real signal.
- **Dust / extreme-edge amounts** rounding to equal integers by
  coincidence — rare, but check trade size before treating a single hit
  as systemic.

## Related

- Implementation: `internal/storage/timescale/trades.go`
  (`isDexUnitRatioTrade`, `InsertTrade`, `BatchInsertTrades`).
- Metric: `stellarindex_dex_trade_unit_ratio_total` —
  `docs/reference/metrics/README.md`.
- Sibling silent-mispricing detector: `dex-nonstandard-decimals.md`
  (decimals-assumption landmine — same "presence != plausibility" class
  of bug).
- `price-divergence.md` — the downstream symptom this can cause on a
  liquid pair.
- ADR-0033 (completeness verification) — what this alert complements:
  completeness proves row presence, not economic correctness.
- CLAUDE.md "Soroban DeFi contracts upgrade in place" — the schema-drift
  trap that most often produces this class of decoder bug.

## Changelog

- 2026-07-09 — initial draft alongside the unit-ratio sentinel.
  Founding incident: the 2026-07-07 Phoenix decoder field-mapping bug
  (237k trades collapsed to an exact 1:1 price for months).
