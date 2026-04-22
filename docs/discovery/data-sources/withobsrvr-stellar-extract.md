# withObsrvr — stellar-extract

**Status:** ✅ **Use as a direct dependency** — pinned to a specific
commit SHA. Best-quality extraction library we've found so far; covers
~80 % of our bronze→silver mapping.

**Repo:** <https://github.com/withObsrvr/stellar-extract>
**Verified against:** `extract.go`, `trades.go`, `scval_converter.go`,
`effects.go`, `go.mod`, `README.md` at clone time (2026-04-22).

## What it is

A Go library. Input: `xdr.LedgerCloseMeta` + network passphrase. Output:
22 typed row slices (see below) ready for Parquet / Postgres / protobuf.

```go
// From go.mod (only one external dep beyond std lib)
require github.com/stellar/go-stellar-sdk v0.5.0
```

Pure library — no binaries to run, no orchestration, no goroutines
beyond the fan-out inside `ExtractAll`. Everything is deterministic
per-ledger and re-runnable.

## Row types emitted (confirmed from `extract.go:51-74`)

```
Ledgers            Transactions      Operations        Effects
Trades             Accounts          Offers            Trustlines
AccountSigners     ClaimableBalances LiquidityPools    ConfigSettings
TTLEntries         NativeBalances    ContractEvents    ContractData
ContractCode       ContractCreations TokenTransfers    EvictedKeys
RestoredKeys
```

This is a superset of what we need for pricing. We'll mostly care
about: `Trades`, `Operations`, `Effects`, `LiquidityPools`,
`ContractEvents`, `TokenTransfers`.

## Entry points

```go
// From raw XDR bytes (streaming ingest / gRPC path)
input, err := extract.NewLedgerInputFromXDR(xdrBytes, networkPassphrase)

// From already-decoded LedgerCloseMeta (Galexie / ingest-SDK path)
input := extract.NewLedgerInput(lcm, networkPassphrase)

// Run every extractor in parallel, collect errors
data, errs := extract.ExtractAll(input)

// Or pull just what we need
trades, err := extract.ExtractTrades(input)
lps,    err := extract.ExtractLiquidityPools(input)
evts,   err := extract.ExtractContractEvents(input)
```

`ExtractAll` uses a panic-recovering goroutine per extractor and
aggregates errors into a slice without failing-fast. Good for
long-running jobs where one malformed ledger shouldn't kill the batch.

## What's *correct* in their implementation

Verified from source:

### Orderbook trade extraction (`trades.go`)

- Only processes **successful** transactions (`tx.Result.Successful()`,
  line 40).
- Extracts from `ManageOfferSuccessResult.OffersClaimed[]` — the
  **actual fills**, not the offer's asked price. This is the path that
  `cdp-pipeline-workflow/processor_transform_to_app_trade.go` gets
  wrong.
- Handles all three offer op types: `ManageSellOffer`, `ManageBuyOffer`,
  `CreatePassiveSellOffer` (lines 55-57).
- Handles both `ClaimAtomTypeOrderBook` and `ClaimAtomTypeV0`
  variants (lines 87 and 129) — the historical-protocol variants
  directly relevant to our [protocol-versions.md](../protocol-versions.md)
  research question.

### Liquidity-pool trade extraction (`effects.go`)

- Separate path through Horizon-style "effects":
  `EffectLiquidityPoolTrade` (90-95 set of LP effect codes, lines 72-77).
- `addLPTradeEffect` reads `claim.LiquidityPool.AmountSold` and
  `AmountBought` directly — protocol-18+ liquidity-pool claims are
  handled correctly.

### Path-payment extraction (`effects.go:325-378`)

- `addPathPaymentStrictReceiveEffects` and
  `addPathPaymentStrictSendEffects` convert path-payment results into
  per-hop `ClaimAtom` traversals, then `addTradeEffects` emits
  per-claim trade effects. So path-payment-generated trades are
  captured — but via `effects.go`, not `trades.go`. This is an
  important splitting of concerns we need to keep track of.

### i128 / u128 / i256 / u256 handling (`scval_converter.go`)

- `int128ToString(val xdr.Int128Parts)` — correct two's-complement
  sign handling:

  ```go
  hi := big.NewInt(0).SetUint64(uint64(val.Hi))
  lo := big.NewInt(0).SetUint64(uint64(val.Lo))
  if uint64(val.Hi)&(uint64(1)<<63) != 0 {
      hi.Sub(hi, big.NewInt(1).Lsh(big.NewInt(1), 64))  // i128 negative
  }
  hi.Lsh(hi, 64)
  hi.Add(hi, lo)
  return hi.String()
  ```
- `uint128ToString` — simple `hi<<64 + lo`.
- `uint256ToString` / `int256ToString` — 4-word shift with sign
  handling on the top word.
- `ConvertScValToJSON` returns a map with **both** raw `hi`/`lo` **and**
  the stringified `value` for auditability.

This is the right model. It's also what lets us confidently reuse this
library under our i128-no-truncation invariant
([decisions.md](../decisions.md)).

## What's *not* covered in their implementation

We'll need to add:

1. **Canonical DEX-trade consolidator.** `Trades` (orderbook only) and
   the LP/path-payment effects are in separate slices. Our internal
   canonical `Trade` record should union them. Not hard — just a
   mapping function.
2. **Soroswap / Aquarius event decoders.** `ExtractContractEvents`
   emits generic `ContractEventData` — it doesn't decode Soroswap's
   `Swap` / `Sync` / `new_pair` events into typed Soroswap records.
   We write our own decoder on top.
3. **Reflector oracle parsing.** Same — their `ContractEventData` is
   the raw event topic+data. SEP-40-aware decoders are ours to build.
4. **Blend event decoding.** Same pattern.

## Stability / release health

- **Pre-1.0** — no semver guarantees yet. We pin a specific SHA and
  upgrade deliberately.
- `go-stellar-sdk v0.5.0` pinned — newer than Galexie's `v0.4.0`. Need
  to check compatibility when we use both in one module. Likely fine
  (both on the `v0` track) but verify.
- Active repo (recent commits).

## Adoption plan

1. `go get github.com/withObsrvr/stellar-extract@<sha>` pinned at our
   first build. Record the SHA in a `VERSIONS.md`.
2. Write a thin wrapper `internal/extract` that:
   - Delegates ledger → typed-row extraction to stellar-extract.
   - Consolidates `Trades` + trade-effects into our canonical
     `CanonicalTrade` type.
   - Adds Soroswap/Aquarius/Blend/Reflector decoders as they're needed.
3. Unit-test the wrapper with fixtures covering each i128 corner case
   (see [decisions.md](../decisions.md)).
4. Revisit upstream SHA on every Stellar protocol upgrade — the
   library tracks `go-stellar-sdk` which tracks protocol changes.

## Open items

- [ ] Confirm whether their `ExtractContractEvents` preserves full
      `xdr.ScVal` for the event's topic+data, or whether it lossily
      JSON-serialises like `ConvertScValToJSON` does in
      `scval_converter.go`. If the latter, we need to take the path
      that preserves raw XDR; otherwise we lose the very precision we
      just promised to preserve.
- [ ] Verify `TokenTransferData` extraction doesn't hit the int64-low-
      bits-only trap — this is the most important field for us.
- [ ] Add a test case that a `u64` amount (native XLM stroops) and an
      `i128` amount (Soroban token) both round-trip losslessly through
      the library → our wrapper → `NUMERIC` column → JSON API → re-read.

## References

- Repo: <https://github.com/withObsrvr/stellar-extract>
- Depends on: `go-stellar-sdk` — see [stellar-archivist.md](stellar-archivist.md)
  and [galexie.md](galexie.md) for the SDK migration context.
- Consumers: nebu origins, `obsrvr-bronze-copier`, flowctl processors.
