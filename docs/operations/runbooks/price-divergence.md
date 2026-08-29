---
title: Runbook — price-divergence
last_verified: 2026-08-29
status: current (the two `stellarindex_price_divergence_*` alerts are INERT)
severity: P2
---

# Runbook — `stellarindex_price_divergence_warning` / `_critical`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_price_divergence_warning` (> 5 %, `for: 2m`, informational) / `_critical` (> 10 %, `for: 2m`, ticket) — **both INERT, see below** |
| Severity | P2 (ticket at 10 %) / P3 (informational at 5 %) |
| Detected by | **Not by those two rules.** They select `stellarindex_our_price` / `stellarindex_reference_price`, which nothing exports; they are kept in `deploy/monitoring/rules/divergence.yml` for intent and tracked in `scripts/ci/lint-metric-refs.sh`'s `KNOWN_INERT` list. Real detection is (a) `flags.divergence_warning` on `/v1/price` + the `divergence_observations` rows behind it, and (b) the LIVE refresh alerts `stellarindex_divergence_refresh_error_dominant` and `stellarindex_divergence_no_reference` (both ticket, `for: 30m`). |
| Typical MTTR | 30 min – hours (depends on cause) |
| Impact | Our aggregated price disagrees with a trusted reference. If we're wrong, every API consumer gets a wrong price — downstream wallets may display misleading USD values, and cross-reference sanity is one of our correctness guarantees. |

> **How you actually learn about a divergence.** Nothing pages on the Δ%
> itself. You find out from `flags.divergence_warning` in an API response, from
> the `/v1/divergence` board and `/v1/divergence/series`, or from the two
> refresh alerts above firing because the checker went blind. Treat this page
> as the investigation guide, not as the trigger's documentation.

### The reference set, as shipped

`divergence_observations.reference` is constrained by migration 0019, widened
by 0148, and mirrored in `internal/api/v1/anomalies.go::divergenceReferences`:

`chainlink`, `coingecko`, `reflector-cex`, `reflector-fx`, `reflector-dex`,
`redstone`, `band`, `synthetic-usd-cross`.

`synthetic-usd-cross` is the derived XLM/USD × USD/fiat cross added in 2026-08
(migration 0148) so EUR/GBP-quoted pairs reach the divergence trust floor.
**CoinMarketCap is not in that set** — there is a CMC *venue poller*
(`internal/sources/external/coinmarketcap`, aggregator class, disabled by
default), but no CMC divergence reference exists;
`internal/divergence/doc.go` still calls it "a candidate future HTTP
reference". Do not go looking for a CMC feed to pull out of rotation.

## Symptoms

- `flags.divergence_warning = true` on `/v1/price` responses for the
  affected asset, driven by a `divergence_observations` row with
  `status = 'firing'` (`|delta_pct| > 5` warning / `> 10` critical).
  There are no `stellarindex_our_price` / `stellarindex_reference_price`
  Prometheus gauges — the per-tick deltas live in the
  `divergence_observations` hypertable (migration 0019), and the
  boolean flag is the public surface.
- Dashboard *Divergence → per-asset* panel shows the spread.
- Often *doesn't* fire alone: bad decimal handling produces 100×
  or 1e6× divergence, not 5–10 %.

## Quick diagnosis (≤ 5 min)

```sh
# Which asset + reference are firing right now, and by how much?
psql -d stellarindex -c \
  "SELECT asset_id, quote_id, reference, our_price, ref_price, delta_pct, observed_at
   FROM divergence_observations
   WHERE status = 'firing'
     AND observed_at > now() - interval '15 minutes'
   ORDER BY abs(delta_pct) DESC LIMIT 10;"

# Compare our live price against the reference side by side.
# (XLM is the canonical `native`; the API listens on :3000.)
curl -s 'http://localhost:3000/v1/price?asset=native&quote=fiat:USD'
curl -s 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'

# What sources are contributing to our aggregate for this asset?
psql -d stellarindex -c \
  "SELECT source, count(*) AS trades, max(ts) AS latest
   FROM trades
   WHERE (base_asset IN ('native', 'crypto:XLM')
          OR quote_asset IN ('native', 'crypto:XLM'))
     AND ts > now() - interval '1 hour'
   GROUP BY source ORDER BY trades DESC;"
```

## Typical root causes (roughly in order of severity)

1. **Stale source contributing to the aggregate.** A source that
   stopped ingesting is still showing up in our VWAP because we
   haven't aged it out of the aggregation window.
   - Signal: one source's `max(observed_at)` is well behind the
     others; that source's price differs from the market by > 5 %.
   - Mitigation: cross-check with `source-stopped.md`; if the
     source is stopped, its historical trades keep contributing
     for the aggregation window — which is correct by design but
     means the divergence will persist until the window rolls
     forward. If it's chronically stopped, disable it in
     `ingestion.enabled_sources`.

2. **Wrong decimals somewhere.** ADR-0003 forbids float and
   requires `NUMERIC`; an accidental `int64(parts.Lo)` truncation
   in a decoder produces a price off by 2^N — catastrophic and
   obvious. This triggers critical divergence and should already
   have been caught by golden-file tests in `internal/sources/*/`.
   - Signal: ratio > 100× or ~0 for the whole asset on one source.
   - Mitigation: revert the decoder PR. This is a data-integrity
     incident; also audit any trades ingested with that version
     and delete them from the trades hypertable before they poison
     aggregates further.

3. **Illiquid market / genuine price discovery.** A thinly-traded
   long-tail token can genuinely move 5–10 % while reference data
   hasn't updated (reference sources don't track every Stellar
   asset).
   - Signal: low trade count in the window; no other sources to
     cross-check against; no other asset from the same issuer
     diverges.
   - Mitigation: nothing — this is a real signal, not an incident.
     Demote to informational via a label-specific silence if it's
     sustained.

4. **Reference source itself is broken.** CoinGecko outage or 429,
   Chainlink-HTTP stale feed, a Reflector/Redstone/Band feed frozen
   on-chain.
   - Signal: *every* asset diverges from that one reference,
     while other references agree with us. If **all** of them go dark
     for a pair you get `stellarindex_divergence_no_reference` instead
     (see [divergence-no-reference](divergence-no-reference.md)).
   - Mitigation: disable that reference in `[divergence]` config and
     restart the aggregator + API until it recovers. Note the on-chain
     oracle references are read from our own ingested `oracle_updates`
     rows, so a frozen feed there is an ingestion question too
     ([oracle-stale](oracle-stale.md)).

5. **Arbitrage window.** Stellar DEX briefly trades at a different
   price from CEX due to cross-chain bridging delays or native
   Stellar liquidity being thin. Real divergence, not a bug.

## Mitigation

- [ ] Step 1 — eyeball the order of magnitude. > 100× divergence
      is always a decoder bug, never a market move — skip to step 3.
- [ ] Step 2 — identify the contributing source(s) and whether one
      is stale (diagnosis above).
- [ ] Step 3 — if decoder bug: revert, audit, purge corrupt rows,
      redeploy. This becomes an incident postmortem automatically.
- [ ] Step 4 — if stale-source: confirm via `source-stopped.md`
      diagnosis and mitigate there.
- [ ] Step 5 — if real market move: document in `postmortems/` as
      a known event (so we don't re-investigate the same spike).
- [ ] Verification: divergence drops under 5 % (or under 10 % for
      the critical rule) and the alert clears.

## Root cause analysis

- Which asset + reference pair + time window.
- Contributing sources + their trade counts + their latest
  observations.
- A sample of raw trade rows in the aggregation window (their
  prices — are they reasonable?).
- Cross-check: two other reference sources agree or disagree?

## Known false-positive patterns

- **CEX-halted vs DEX-open markets** (e.g., during CEX maintenance):
  our DEX data keeps moving; CoinGecko's aggregate lags. Real
  divergence that isn't our fault.
- **Freshly-listed tokens** — reference sources don't track them,
  so their "reference price" is the issuer's advertised number or
  empty. Suppress for new listings by excluding them from the
  divergence feed for the first 24 h.

## Related

- `divergence-refresh-error-dominant.md` / `divergence-no-reference.md` — the
  two alerts that DO fire on this surface.
- `oracle-stale.md` — when the reference *is* an on-chain oracle.
- `source-stopped.md` — a stopped source is the most common
  divergence cause we'd actually do something about.
- ADR-0003 (i128 precision) — decoder discipline that prevents
  decimal-related divergence.
- `divergence.yml` alert rules — if you retune thresholds, update
  this runbook too. If a producer for `stellarindex_our_price` /
  `stellarindex_reference_price` ever ships, drop the KNOWN_INERT entry
  and this whole banner with it.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): At-a-glance
  presented the `stellarindex_price_divergence_warning`/`_critical` pair as
  live detection while Symptoms already said the gauges don't exist — the two
  rules are KNOWN_INERT (no producer) and the real signals are
  `flags.divergence_warning` / `divergence_observations` plus the live
  `divergence_refresh_error_dominant` + `divergence_no_reference` alerts;
  CoinMarketCap was listed as an active reference but has no divergence
  implementation — replaced with the shipped set (Chainlink, CoinGecko,
  Reflector CEX/FX/DEX, Redstone, Band, and `synthetic-usd-cross` since
  migration 0148).
- 2026-06-12 — F-1330: rewrite diagnosis to executable form — there
  are no `stellarindex_our_price`/`_reference_price` gauges (deltas
  live in `divergence_observations`, mig 0019); API port is :3000;
  XLM is `native`; trades columns are `ts`/`base_asset`/`quote_asset`.
- 2026-04-23 — initial draft. Lays out the "order of magnitude"
  triage trick: > 100× is always a decoder bug.
