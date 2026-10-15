---
title: USD volume coverage — 100% CEX / 99.5% SDEX
last_verified: 2026-10-06
status: IMPLEMENTED + verified live (v0.19.0); ratio served on /v1/coverage as usd_volume_pricing
---

# USD volume coverage

## The bar (operator)

- **100%** of external-exchange (CEX/FX) trades priced in USD.
- **99.5%+** of SDEX trades priced in USD.
- Unpriced is acceptable ONLY for genuinely illiquid Stellar tokens, as a
  visible, measured exception, never a silent NULL.

## Outcome (measured live, v0.19.0)

Coverage of trades in the first hours after deploy vs the 2026-07-17 baseline:

| source | before | after | bar |
|---|---|---|---|
| binance | 98.626% | **100.000%** | 100% |
| coinbase | 96.319% | **100.000%** | 100% |
| kraken | 78.118% | **100.000%** | 100% |
| bitstamp | 91.273% | **100.000%** | 100% |
| sdex | 83.848% | **99.734%** | 99.5% |

The remaining unpriced tail is intended: obscure tokenised gold/equity wrappers
(`USGOLD/GOLDRESERVE`, `HD/IBM`, `RUGOLDMINE/GOLDRESERVE`, `ATME/FiaT`) with no
XLM market or priceable counterparty, plus pure SEP-41/SEP-41 pairs, excluded
**deliberately**: `decimals()` is per-contract and not plumbed through the
trade-insert path, so assuming 1e7 would be a silent money bug, worse than NULL.

## Why it matters

`usd_volume` is the denominator of every volume surface; a NULL silently
deflates every aggregate built on it. Before v0.19.0, 7.06% of a day's trades had
NULL `usd_volume` (~$939M against $3.167B counted, ~23% understated), unevenly
by venue (1.4% of binance trades vs 21.9% kraken vs 65.5% aquarius). Cause: the
fiat-quote tier looked up `<asset>/<peg>` in `prices_1m`, which has no fiat/fiat
rows; the rate lives in `fx_quotes` (daily, back to 2001-05-11), which the insert
path (`tradeUSDVolume`, `internal/storage/timescale/trades.go`) never read.

## Plan (all shipped v0.19.0 unless marked)

- **Tier 2a, fiat quotes via `fx_quotes`:** multiply a `fiat:*` quote by
  `inverse_usd` at-or-before the trade timestamp (`FXQuoteAtOrBefore`). All of
  the 2026-07-17 external-exchange NULLs were fiat-quoted, so this alone took
  the four CEX venues to 100%.
- **Tier 2b, base-side pegs:** a USD-pegged BASE gives `usd_volume = base_amount`.
- **Tier 3b/4b, XLM bridge, both legs (path to 99.5%):** value via
  `TOKEN/XLM × XLM/USD` at trade time; prefer the deeper XLM market; the bridge
  quote must be non-dust. Quote leg reached 87.5% of the remaining unpriced;
  adding the BASE leg reached **99.2%**. The bridge has a 24h freshness window,
  not the direct market's 1h: the tokens it prices trade $8-$220 per day, so a
  stale rate is a rounding error while a NULL drops the row.
- **Tier 4, direct USD price for either side:** any USD price at that timestamp
  (CEX, oracle, SDEX USD-peg market).

### Measurement + enforcement

`stellarindex_trade_inserts_total{source,usd_volume_populated}` is emitted in
`InsertTrade`/`BatchInsertTrades`, the choke point every trade passes once.
Alerts `stellarindex_cex_usd_volume_coverage_low` and
`stellarindex_onchain_usd_volume_coverage_low` enforce the two bars
(`configs/prometheus/rules.r1/usd-volume-coverage.yml`); they count priced
against unpriced rows, so a pegged asset sliding into the FX-bridged `estimated`
tier moves neither. `verify-usd-volume.timer` (05:12 UTC, `-days 1`) re-checks
yesterday's exact tiers and writes `stellarindex_verify_usd_volume_*` to the
node_exporter textfile collector.
`stellarindex_usd_volume_estimated_tier_high` tickets when the estimated tier
exceeds 5% of a day's valued volume (measured 1.3-2.7% over the 11 days to
2026-10-14; the dollar sum swings $29M-$727M with activity);
`stellarindex_usd_volume_xlm_bound_skipped` tickets when the run could not build
the day's XLM/USD rate from `prices_1h` and so left the estimated tier unjudged;
`stellarindex_verify_usd_volume_stale` tickets when no run has stamped
`last_run_unix` in 30 h. An exact-tier violation or crash exits non-zero and
opens `stellarindex_systemd_unit_failed`, deliberately without ride-out.

`GET /v1/coverage` publishes `usd_volume_pricing` (per-source priced / unpriced /
unroutable counts over 24h, `priced_ratio`, `bar`, `meets_bar` for external
venues only, `lower_bound`); it never changes `complete` or `lake_complete`
(ADR-0033). `scripts/ops/pre-launch-check.sh` gates go-live on any external venue
with `meets_bar: false` and any firing `usd-volume-coverage` alert.

### Backfill

Rows older than the v0.19.0 deploy keep their NULLs until re-derived with
`stellarindex-ops usd-volume-restamp` at point-in-time rates (procedure:
[usd-volume-rederive-2026-08.md](usd-volume-rederive-2026-08.md); INV-0974 done).
`fx_quotes` reaches 2001, so FX history does not limit it.

## Correctness constraints

- **Point-in-time rates, always.** Value each trade at its ledger close time,
  never spot, or historical volumes rewrite as rates drift.
- **Do not retroactively change a depeg.** Insert-time behaviour trusts the peg
  and records the observed quote amount; keep that.
- **Bridge quotes must be non-dust** so a 2-stroop crumb can't set a valuation
  rate (see `finding-dust-trades-set-chart-extremes.md`).

## SAC wrappers and fiat-pegged classics

Soroban DEX sources (Soroswap, Phoenix, Aquarius, Comet) emit
`base_asset`/`quote_asset` as the SAC contract ID. Without an operator-config
mapping from C-strkey to "CODE-ISSUER", the explorer shows raw C-strkeys,
`/v1/sac-wrappers` is empty, and `trades.usd_volume` stays NULL because the
on-chain path can't follow `quote_asset = C…` to a USD-pegged classic. A config
line fixes new trades; a backfill prices historical rows.

### Adding a single SAC mapping

#### 1. Resolve the SAC's underlying classic

```sh
curl https://api.stellar.expert/explorer/public/contract/<C-strkey> \
  | jq -r '.asset'
```

Returns `CODE-ISSUER-N`; strip the trailing `-N` (stellar.expert's display
ordinal) and sanity-check the issuer's home_domain.

#### 2. Append to `[supply.sac_wrappers]` on r1

```sh
ssh root@136.243.90.96 'cat >> /etc/stellarindex.toml' << 'EOF'
"<C-strkey>" = "<CODE>:<G-strkey>"
EOF
```

Separator is a **colon**, not the dash of the canonical `/v1/assets` asset_id.

#### 3. Restart the api + indexer + aggregator

```sh
ssh root@136.243.90.96 'systemctl restart stellarindex-api stellarindex-indexer stellarindex-aggregator'
```

Verify:

```sh
curl -s https://api.stellarindex.io/v1/sac-wrappers | jq '.data | length'
```

#### 4. Bake into the ansible template

Append the entry to the `[supply.sac_wrappers]` block in
`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2` in the
same PR so re-renders don't lose it.

### Backfilling historical USD-volume

New trades populate `trades.usd_volume` once a USD-pegged SAC entry lands;
earlier trades stay NULL. Price them with `stellarindex-ops usd-volume-restamp`
`-tier exact -fill-null`. A SAC-quoted trade whose wrapper resolves to a
USD-pegged classic is exact-tier (`quote_amount / 10^7`); the tool classifies
from the same `[trades].usd_pegged_classic_assets` and `[supply.sac_wrappers]`
the insert path reads, so there is no hand-kept `IN (…)` list.

```sh
# dry run: per-day candidate counts, nothing written
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier exact -fill-null -sources aquarius,soroswap,phoenix,comet \
  -from <first-day> -to <last-day>
# apply on r1 under the heavy wrapper; add -chunks for any window older
# than the trades compression policy's 15 days
set -a; . /etc/default/stellarindex; set +a
/usr/local/sbin/run-heavy-job.sh usd-sac-fill \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier exact -fill-null -chunks \
    -sources aquarius,soroswap,phoenix,comet \
    -from <first-day> -to <last-day> -write
```

Then run the windowed CAGG refreshes the tool prints (it refreshes nothing; every
served volume surface reads a CAGG). Full procedure: [usd-volume-rederive-2026-08.md, Step
5](usd-volume-rederive-2026-08.md).

Do NOT backfill with a hand-written `UPDATE trades … WHERE usd_volume IS NULL`.
`trades` is a compressed hypertable; an UPDATE with no `ts` predicate makes every
chunk a result relation in one transaction (measured: 260 result relations,
~270 GB WAL on a 49 GB root fs) and leaves `derive_generation` at 0, so a later
live re-write reverts it. The tool slices by `ts`, lifts the decompression cap
with `SET LOCAL` per slice and stamps the run's generation.
`scripts/ci/lint-migration-commands.sh` fails an ops SQL script that runs DML on
a compressed hypertable without a time-column predicate.

### Adding a new USD-pegged classic

For a SAC pointing at a NEW USD-pegged stablecoin (not just USDC), also extend
`[trades].usd_pegged_classic_assets` in `/etc/stellarindex.toml`:

```toml
[trades]
usd_pegged_classic_assets = [
  "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
  "USDx-GAVH5ZWACAY2PHPUG4FL3LHHJIYIHOFPSIUGM2KHK25CJWXHAV6QKDMN",  # NEW
]
```

Trades quoted in USDx (or its SAC) are then priced `usd_volume = quote_amount /
10^7`: new ones at insert, historical ones by the `usd-volume-restamp` run above.

### Fiat-pegged classics on the trade path

`[pricing_guard].fiat_pegged_classic_assets` (asset_key to ISO-4217 ticker) is
the operator's declaration that a classic asset is 1:1-pegged to a fiat
currency. The API already reads it for the `declared_peg` price fallback; the
trade path reads the same map, so a DEX trade with a declared leg (classic or
its SAC) is valued as `amount / 10^7 × fiat→USD rate` at trade time instead of
through the counterparty-authorable FX bridge.

The tier sits after the XLM anchors and before the estimated FX tier. It honours
the declaration only while the declared classic asset's own on-chain USD price is
within 200 bps (`fiatPegToleranceBps` in `internal/storage/timescale/trades.go`)
of the fiat rate; the classic form is priced even when the leg is the SAC, since
the resolver links a SAC to its markets only through `[supply].sac_wrappers`. A
depeg, a missing on-chain price or a missing fiat rate declines, and the leg
falls through to the FX tier, never above it. The value takes the FX tier's leg
cross-check and single-leg cap and stays in the `estimated` reconcile tier, so
`ClassifyUSDVolumeTier`, the exact restamp lane and `verify` are unchanged. It
needs `usd_pegged_classic_assets`: `InstallUSDVolumeResolution` refuses fiat
pegs on their own.

AUDD and AUDR sit in the same map; the substance gate withholds their on-chain
prices, so the tier declines them. Adding an asset is one line in the
`[pricing_guard.fiat_pegged_classic_assets]` stanza. The tier is forward-only:
no restamp lane reaches it, so stored rows keep their FX-tier value (within the
2% band where the declared leg was the quote; the old quote-priced value where
it was the base), and rows the FX tier left NULL stay NULL.

### Pure-Soroban SEP-41 tokens (no USD-pegged quote at all)

The paths above need the QUOTE asset to resolve to a USD-pegged classic. A pure
SEP-41 token routed only against XLM has none, so two further tiers apply, both
gated on `[trades].usd_pegged_classic_assets` being non-empty (which also wires
`VWAPUSDFXResolver`, see `cmd/stellarindex-indexer/main.go`):

- **Tier 3**: the quote asset, including plain `native` XLM, has a recent VWAP
  against a configured USD peg in `prices_1m`. Covers `base=TOKEN, quote=XLM`.
- **Tier 4**: tier 3 declined AND the BASE is `native` XLM or its SAC. Covers
  `base=XLM, quote=TOKEN` (decoders don't re-orient trades). Values off the XLM
  leg, `usd_volume = base_amount/1e7 × XLM/USD`, needing no token decimals.

Both tiers are insert-time only (no retroactive backfill). A SEP-41/SEP-41 pair
(neither leg XLM nor USD-pegged) stays out of scope on every tier: it needs a
per-token oracle. `tradeUSDVolume`'s docstring in
`internal/storage/timescale/trades.go` is authoritative for the four-tier
order; `Store.SorobanVolume24hUSDForAsset` is the read-side equivalent for
`/v1/assets/{id}`'s `volume_24h_usd`, which also has a query-time fallback for
trades that predate tier 3/4.

## Related

- `internal/ops/chops/usd_volume_restamp.go`: the backfill.
- `internal/storage/timescale/usd_volume_quote_spec.go`: the live USD-volume
  path; restamp classifies through the same spec.
- `internal/api/v1/known_issuers.go`: curated org-name fallback; add an entry
  alongside the SAC for explorer label parity.
