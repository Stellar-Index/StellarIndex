# withObsrvr — cdp-pipeline-workflow

**Status:** ❌ **Do not fork. Do not adopt as a dependency.** Legacy
monolith with multiple correctness bugs in the exact areas we care
about (SDEX trades, Soroswap swaps). Useful only as a roadmap of *what
an all-in-one pipeline looks like* — the individual components are not
trustworthy for our SLAs.

**Repo:** <https://github.com/withObsrvr/cdp-pipeline-workflow>
**Verified against:** `factory.go`, `source_adapter.go`,
`source_adapter_stellar_rpc.go`,
`processor/processor_transform_to_app_trade.go`,
`processor/processor_soroswap_router.go`,
`consumer/consumer_save_to_timescaledb.go`, `CLAUDE.md` at clone time
(2026-04-22).

## Scope of the repo

A single Go binary that composes sources, processors, and consumers via
a YAML config. The factory in `factory.go` enumerates:

- **9 source adapters** — `CaptiveCoreInboundAdapter`,
  `BufferedStorageSourceAdapter`, `SorobanSourceAdapter`,
  `GCSBufferedStorageSourceAdapter`, `S3BufferedStorageSourceAdapter`,
  `RPCSourceAdapter`, `BronzeSourceAdapter`, `DuckLakeSourceAdapter`,
  and a deprecated `FSBufferedStorageSourceAdapter` (explicitly errors
  in `factory.go:30-31`).
- **~45 processors** — incl. `TransformToAppTrade`,
  `TransformToTokenPrice`, `TransformToTickerOrderbook`,
  `MarketMetricsProcessor`, `SoroswapRouter`, `Soroswap`,
  `BronzeExtractors`, etc.
- **~35 consumers** — incl. `SaveToTimescaleDB`, `SaveToPostgreSQL`,
  `SaveToClickHouse`, `SaveToRedisOrderbook`, `SaveToParquet`,
  `SaveToDuckDB`, `SaveToWebSocket`, `SaveToZeroMQ`.

Big on breadth. Small on depth.

## Verified correctness bugs

### 1. `TransformToAppTrade` reads the *ask*, not the *fill*

`processor/processor_transform_to_app_trade.go:112-129`:

```go
case xdr.OperationTypeManageBuyOffer:
    offer := op.Body.MustManageBuyOfferOp()
    trade.SellAmount = amount.String(offer.BuyAmount)
    trade.BuyAmount  = amount.String(offer.BuyAmount)
    trade.Price      = float64(offer.Price.N) / float64(offer.Price.D)
    ...
case xdr.OperationTypeManageSellOffer:
    offer := op.Body.MustManageSellOfferOp()
    trade.SellAmount = amount.String(offer.Amount)
    trade.BuyAmount  = amount.String(offer.Amount)
    trade.Price      = float64(offer.Price.N) / float64(offer.Price.D)
    ...
```

The processor pulls price and amounts **from the offer operation
itself** — which is the *request*. The actual matched fills live in
`opResult.MustManageSellOfferResult().MustSuccess().OffersClaimed`,
which this code never reads. As a result:

- Every "trade" emitted is in fact an order placement, whether or not
  it matched.
- The recorded "price" is the asked price, not the executed price.
- Both `SellAmount` and `BuyAmount` are set to the same field, so
  the amounts are wrong at the type level.

Contrast with the correct implementation in
[withobsrvr-stellar-extract.md](withobsrvr-stellar-extract.md) —
`trades.go` walks `offerResult.OffersClaimed[]` and handles
`ClaimAtomTypeOrderBook` / `ClaimAtomTypeV0` variants.

Additionally, the processor doesn't handle
`OperationTypeCreatePassiveSellOffer`, `OperationTypePathPaymentStrictSend`,
or `OperationTypePathPaymentStrictReceive` — all of which produce
claim atoms and therefore trades.

### 2. Soroswap router processor silently truncates i128 amounts

`processor/processor_soroswap_router.go:155-177`:

```go
case "amounts":
    if entry.Val.Vec != nil && len(entry.Val.Vec) >= 2 {
        routerEvent.AmountA = fmt.Sprintf("%d", entry.Val.Vec[0].I128.Lo)
        routerEvent.AmountB = fmt.Sprintf("%d", entry.Val.Vec[len(entry.Val.Vec)-1].I128.Lo)
    }
...
case "amount0", "amount_a":
    routerEvent.AmountA = fmt.Sprintf("%d", entry.Val.I128.Lo)
case "amount1", "amount_b":
    routerEvent.AmountB = fmt.Sprintf("%d", entry.Val.I128.Lo)
```

Only the **low 64 bits** of every i128 amount are read; `.Hi` is
ignored. Per our [i128 decision](../decisions.md#2026-04-22--i128--u128-must-survive-end-to-end-with-no-truncation),
this is a correctness bug any time an amount exceeds ~922 B tokens
(i64 max ÷ 10⁷ decimals). This is the **exact same class of bug** that
Stellar Expert has publicly acknowledged in their own DB.

Also — parsing ScVal from a JSON-decoded struct (`I128.Lo uint64`) is
architecturally brittle. They're relying on the upstream to have
pre-decoded the ScVal to JSON; any upstream format change silently
breaks parsing.

### 3. `RPCSource` does not advance its cursor between polls

`source_adapter_stellar_rpc.go:139` — every poll iteration calls
`s.getEvents(ctx, s.config.StartLedger, endLedger, ...)` with the
original `StartLedger`. There is no update to `StartLedger` based on
the cursor returned in the previous response. At best this duplicates
work on every poll; at worst (on a wide filter) it hits rate limits
and falls behind.

### 4. `CaptiveCoreInboundAdapter` has no cursor persistence

`source_adapter.go:120-127`:

```go
latestNetworkLedger := rootHAS.CurrentLedger
...
if err := captiveBackend.PrepareRange(ctx, ledgerbackend.UnboundedRange(uint32(latestNetworkLedger))); err != nil {
    return errors.Wrap(err, "error preparing captive core ledger range")
}
```

On every start it picks up from `latestNetworkLedger` — i.e.
**whatever the current tip is** at the moment of process start. If the
pipeline restarts, the ledgers that closed while it was down are
silently lost. There's no persistent cursor.

Also `UserAgent: "my_app"` is left in the code at line 107 — minor
hygiene issue but indicates the adapter isn't production-hardened.

### 5. `SaveToTimescaleDB` uses row-by-row `INSERT`

`consumer/consumer_save_to_timescaledb.go:94-112` — every trade is a
separate `db.ExecContext(...)` call. No `COPY`, no batching, no
prepared-statement reuse. Expected throughput is far below what
TimescaleDB can sustain.

The schema also has no primary key → re-runs create duplicates. And
the `price` column is `NUMERIC` but the upstream `AppTrade.Price` is a
Go `float64`, so precision is lost before it gets to the column.

### 6. Schema lacks retention / continuous aggregates

No `add_retention_policy()`, no continuous aggregates, no indexes
beyond `(sell_asset, buy_asset)` and `(seller_id, buyer_id)`. The
value-add of TimescaleDB over vanilla Postgres isn't used here.

## What's *good* in the repo (things to borrow as patterns)

1. **Factory-based component registration** (`factory.go`) is a clean
   config-to-component mapping. We can replicate the pattern in our
   own code without adopting their components.
2. **Multi-sink architecture** — they demonstrate that a pipeline can
   fan out to TimescaleDB + Redis + WebSocket in parallel. We'll likely
   want the same.
3. **Explicit legacy vs. enhanced adapter pairs** (`NewS3...Enhanced` /
   `NewS3BufferedStorageSourceAdapter`, with fallback) — acknowledges
   that refactors can land incrementally.
4. Their **own CLAUDE.md gives us useful SDK guidance verbatim**
   (see [withobsrvr-overview.md](withobsrvr-overview.md) — SDK helper
   methods abstract protocol version differences).

## Verdict

**Do not use.** Treat as:

- A **processor-name dictionary** for the conceptual categories we'll
  need (trade, orderbook, market-metric, token-price, soroswap-router,
  contract-event, ledger-change, ...).
- A **pitfall atlas** — every bug above is a concrete test case for our
  own implementation.

Our own pipeline takes the clean patterns from `nebu` + `stellar-extract`
and leaves the broken ones here.

## References

- Repo: <https://github.com/withObsrvr/cdp-pipeline-workflow>
- `stellar-extract` (where the corrected logic lives): [withobsrvr-stellar-extract.md](withobsrvr-stellar-extract.md)
- `nebu` (where the cleaner contract lives): [withobsrvr-nebu.md](withobsrvr-nebu.md)
- i128 decision: [../decisions.md](../decisions.md)
