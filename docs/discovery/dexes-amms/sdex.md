# Stellar Classic DEX (SDEX) & Liquidity Pools

**Status:** ✅ Core trade source. Extractable end-to-end via
`stellar-extract` + our own consolidator.

**Verified against:**
- `stellar-docs/docs/learn/fundamentals/liquidity-on-stellar-sdex-liquidity-pools.mdx`
- `go-stellar-sdk/xdr/xdr_generated.go:37430-37900` (`ClaimAtomType`,
  `ClaimOfferAtomV0`, `ClaimOfferAtom`, `ClaimLiquidityAtom`)
- `withObsrvr/stellar-extract/trades.go` (correct orderbook extraction)
- `withObsrvr/stellar-extract/effects.go` (correct LP + path-payment
  extraction)

## What SDEX is

Two mechanisms coexist natively in stellar-core and produce trades:

1. **Orderbook DEX** — on-chain price-time-priority limit-order book
   per asset pair. All orders stored as *selling* obligations (buy
   orders are internally converted to sells at the inverse price).
2. **AMM / Liquidity Pools** — constant-product pools
   (`x · y = k`) introduced in Protocol 18. Fee: 30 bps baked in.

Both are addressable via the same `PathPaymentStrict{Send,Receive}` op —
the protocol automatically routes a single path payment through the
combined liquidity of orderbook + pools for best execution.

## Every trade in SDEX leaves a `ClaimAtom`

This is the key insight and the reason why our pricing engine must
read from **operation results**, not operation requests. A trade only
happens if a counterparty exists; the operation body (the offer
request) records *what the user asked for*, but the set of
`ClaimAtom`s in the result records *what actually happened*.

`withObsrvr/stellar-extract/trades.go` (which we plan to depend on)
does this correctly. `cdp-pipeline-workflow/processor_transform_to_app_trade.go`
does it wrong — see [data-sources/withobsrvr-cdp-pipeline-workflow.md](../data-sources/withobsrvr-cdp-pipeline-workflow.md)
for why.

## Trade-producing operations

Five operation types produce `ClaimAtom`s:

| Op type                       | Result struct → claim atoms location                                          |
| ----------------------------- | ------------------------------------------------------------------------------ |
| `ManageSellOffer`             | `ManageSellOfferResult.Success.OffersClaimed[]`                                |
| `ManageBuyOffer`              | `ManageBuyOfferResult.Success.OffersClaimed[]`                                 |
| `CreatePassiveSellOffer`      | `CreatePassiveSellOfferResult.Success.OffersClaimed[]` (uses `ManageOfferSuccessResult`) |
| `PathPaymentStrictSend`       | `PathPaymentStrictSendResult.Success.Offers[]`                                 |
| `PathPaymentStrictReceive`    | `PathPaymentStrictReceiveResult.Success.Offers[]`                              |

Our pricing pipeline **must cover all five**. `stellar-extract/trades.go`
covers the first three via the `ManageOfferSuccessResult` path;
`stellar-extract/effects.go` covers the two `PathPayment` variants via
the effects path. Our consolidator unifies both into one canonical
`Trade` row.

**Passive** sell offers are a minor wrinkle: they won't cross an
equal-priced counter-offer, they only fill on an unequal price. For
us this is just an additional op type to include in the extractor —
the claim atoms are structurally identical to `ManageSellOffer`'s.

## The three `ClaimAtom` XDR variants

Verified from `go-stellar-sdk/xdr/xdr_generated.go:37430-37900`:

### `ClaimAtomTypeClaimAtomTypeV0 = 0` — pre-fix legacy

```xdr
struct ClaimOfferAtomV0 {
    uint256 sellerEd25519;    // raw Ed25519 public-key bytes (no muxed variant)
    int64   offerID;
    Asset   assetSold;
    int64   amountSold;
    Asset   assetBought;
    int64   amountBought;
}
```

Used by pre-upgrade ledgers. Our historical backfill **must** handle
this variant. Conversion: derive the G… address from the 32-byte
`sellerEd25519` using the standard SEP-23 / strkey algorithm.

### `ClaimAtomTypeClaimAtomTypeOrderBook = 1` — modern orderbook fill

```xdr
struct ClaimOfferAtom {
    AccountID sellerID;
    int64     offerID;
    Asset     assetSold;
    int64     amountSold;
    Asset     assetBought;
    int64     amountBought;
}
```

### `ClaimAtomTypeClaimAtomTypeLiquidityPool = 2` — AMM fill (P18+)

```xdr
struct ClaimLiquidityAtom {
    PoolID liquidityPoolID;
    Asset  assetSold;
    int64  amountSold;
    Asset  assetBought;
    int64  amountBought;
}
```

LP claims have a `PoolID` (32-byte SHA-256 over the pool parameters)
instead of an offer/seller — the "seller" for the AMM leg of a trade
is the pool itself, not an account.

### Common structure

All three carry:

- `AssetSold`, `AssetBought` (XDR `Asset` — native / alphanum4 /
  alphanum12).
- `AmountSold`, `AmountBought` as **`Int64`**, i.e. raw stroops with
  7 implied decimals (classic assets are always 7-decimal).

**Important i128 note:** classic `ClaimAtom` amounts are `Int64`, not
`Int128`. The i128 concern from [decisions.md](../decisions.md)
applies to Soroban-origin amounts (SAC balances, contract swap
events), **not** to these classic SDEX claim atoms. But our storage
schema must still be `NUMERIC` uniformly, because our unified `Trade`
table holds both classic and Soroban trades.

## Price derivation

The "price" of a classic SDEX offer op body is stored as
`{numerator, denominator}` with both as 32-bit signed ints
(`Int32`). This is the **requested** price. It is **not** the price
for our aggregation — we derive executed price directly from the
claim atom amounts:

```
executed_price_bought_per_sold = amount_bought / amount_sold
executed_price_sold_per_bought = amount_sold / amount_bought
```

Both computed as `big.Rat` (exact rational) or `decimal.Decimal` at
sufficient precision. Never float64.

For USD normalisation we route through our broader pricing layer (not
intra-SDEX math alone), per the ctx-proposal.md aggregation strategy.

## Liquidity-pool specifics

- Pool type: only `LIQUIDITY_POOL_CONSTANT_PRODUCT` today (SDK enum).
- Pool fee: **30 bps** (`LiquidityPoolFeeV18 = 30`), fixed at protocol
  level for classic pools.
- PoolID: deterministic `SHA-256` over
  `(LiquidityPoolType, AssetA, AssetB, fee)` with A, B in
  **lexicographic order** (`Asset.compare(A, B) <= 0`). Only **one**
  pool per asset pair — no fee-tier splits like Uniswap V3.
- Trustline prerequisites: holder needs trustlines for A, B, **and**
  the pool share asset.

### Reserve-based "spot price" (no trade required)

From the docs:

> `spotPrice = reserveA / reserveB`

Useful for continuous pricing between sparse trade events — matches
the approach our ctx-proposal describes for Soroswap.

For classic LPs we read reserves from `LiquidityPoolEntry.body.constantProduct.reserveA/reserveB`
ledger entries. `stellar-extract/effects.go:181-261` has
`getLiquidityPoolDelta` / `liquidityPoolDetails` helpers we can lift.

### Reserve change → implied TWAP

LP state changes (deposits, withdrawals, trades) change the reserves
and thus the implied price. For a pool-state TWAP over a window:

1. Collect every `LiquidityPoolEntry` pre/post state in every ledger
   that touches the pool.
2. For each change, compute the implied price at that moment.
3. Time-weight across the window.

This is denser computation than purely event-driven indexing — we'll
probably only maintain the most recent reserve snapshot for serving
current prices, and reconstruct historical TWAP on demand from the
bronze layer (Galexie lake) when asked.

## Data flow for our pipeline

```
Galexie → LedgerCloseMeta → ingest.NewLedgerTransactionReaderFromLedgerCloseMeta
                                   │
                                   ├─ stellar-extract.ExtractTrades
                                   │     (ManageOffer / CreatePassive → OrderBook + V0 claim atoms)
                                   │
                                   ├─ stellar-extract.ExtractLiquidityPools
                                   │     (pool state snapshots for implied spot prices)
                                   │
                                   └─ stellar-extract.(Extract)Effects (subset)
                                         (PathPayment trades, LP-trade effects)
                                                │
                                                ▼
                              Our consolidator: canonical CanonicalTrade rows
                                                │
                                                ▼
                              TimescaleDB `trades` hypertable
                                (time, source='SDEX-OB'|'SDEX-LP',
                                 base_asset, quote_asset, amount_sold,
                                 amount_bought, seller, buyer, pool_id,
                                 offer_id, tx_hash, op_index)
```

## SDEX coverage gaps in `stellar-extract`

Flagged in [withobsrvr-stellar-extract.md](../data-sources/withobsrvr-stellar-extract.md),
restated here in SDEX context:

- `ExtractTrades` only looks at `OrderBook` and `V0` ClaimAtom
  variants — not `LiquidityPool` (that path is handled via
  `effects.go:addLPTradeEffect`, a **different slice** of output). We
  consolidate both slices.
- `ExtractTrades` handles only three op types (Manage{Sell,Buy}Offer,
  CreatePassiveSellOffer). `PathPayment` trades are again via
  `effects.go:addPathPaymentStrict{Send,Receive}Effects`. We include
  both paths in our consolidator.

## Asset identity & normalization

For our canonical `Trade` row we store assets in canonical string
form:

- `"XLM"` (or `"native"` depending on convention we pick) for XLM.
- `"{CODE}:{ISSUER_G_ADDRESS}"` for non-native classic assets.
- `"{CONTRACT_C_ADDRESS}"` for Soroban SEP-41 tokens.

`stellar-extract` uses `Asset.StringCanonical()` which yields
`native` or `CODE:ISSUER`. We adopt the same convention for
compatibility. Our API translates into RFP-specified field names
(`Asset/Token Code`, `Issuer Address`, `Contract Address`).

For asset code decoding, both `AlphaNum4` and `AlphaNum12` are
zero-padded byte arrays — we strip trailing `\x00` as
`stellar-extract/trades.go:98` does.

## Historical / since-inception

We can backfill from ledger 2 using Galexie against a fully-catchup
core. Per the [archival-nodes.md](../data-sources/archival-nodes.md)
plan, we `CATCHUP_COMPLETE` a dedicated core and run Galexie against
its output, then point a batch indexer at the resulting data lake to
build the `trades` hypertable from genesis.

## Open items

- [ ] Write the canonical-trade consolidator that merges
      `stellar-extract.Trades` + LP effects + PathPayment effects
      into one SQL-friendly row shape.
- [ ] Add `CreatePassiveSellOffer` to the consolidator — it's
      structurally identical to `ManageSellOffer` but a separate op
      type. Double-check `stellar-extract/trades.go:55-57` (we saw it
      listed there — so probably already covered; verify with a test
      fixture).
- [ ] Confirm offer / amount handling under pre-P13 (multiplexed
      account intro) and pre-P18 (LP intro) protocol eras. The
      ClaimAtomV0 → OrderBook transition point specifically.
- [ ] Decide storage granularity for LP reserve snapshots: every
      ledger with a delta, or only when a trade happens (smaller DB,
      loses depositor-only events useful for analytics).
- [ ] Map reserve snapshots to a TWAP materialisation job (TimescaleDB
      continuous aggregate).
- [ ] Build a fixture set: one ledger containing each of the five op
      types + all three ClaimAtom variants, used as a regression test
      for the consolidator.

## References

- SDEX + LP conceptual doc: `stellar-docs/docs/learn/fundamentals/liquidity-on-stellar-sdex-liquidity-pools.mdx`
- Path Payments guide: `stellar-docs/docs/build/guides/transactions/path-payments.mdx`
- ClaimAtom XDR: `go-stellar-sdk/xdr/xdr_generated.go:37430-37900`
- Correct trade extractor: `stellar-extract/trades.go`
- Correct LP-trade / path-payment extractor: `stellar-extract/effects.go`
- Related: [../decisions.md](../decisions.md) (i128 invariant),
  [../protocol-versions.md](../protocol-versions.md) (epoch handling).
