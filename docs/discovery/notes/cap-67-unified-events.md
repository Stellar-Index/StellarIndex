# CAP-67 Unified Asset Events — reference

**Status:** ✅ Read primary. Has major implications for our ingest
path and for how we unify classic + Soroban token movements in one
stream.

**Source:** `.discovery-repos/stellar-protocol/core/cap-0067.md`
(Status: Final, Protocol version 23, authored by @sisuresh + @leighmcculloch).

## What CAP-67 does (one paragraph)

Starting at **Protocol 23 (Whisk)**, mainnet-activated **2025-09-03**,
classic Stellar operations emit the same kind of contract-events that
SEP-41 tokens and SAC-wrapped classic assets already emit. The five
unified event types are:

```
transfer      mint      burn      clawback      set_authorized
```

Plus a new transaction-level event:

```
fee
```

**Before P23**: classic transfers had no events; you had to parse
operations + effects. Soroban-emitted events existed only for SAC and
custom Soroban tokens.

**After P23**: every asset movement on Stellar — classic or Soroban —
emits a standard event. Indexers can subscribe to one stream.

## Topic schema (verified)

The unified classic-asset events emit with **4 topics**, not 3:

```
[ Symbol <event_name>,
  Address from,
  Address to,
  String  sep0011_asset ]   // SEP-11 canonical asset string
```

Compared to SEP-41 Soroban tokens (3 topics: `event_name, from, to`)
this is an extra `sep0011_asset` topic encoding the asset identity
at the end. For SAC contracts the asset identity is implicit in the
contract address (topic arg 0 of the emitted event carries the
contract_id via Soroban's event envelope), but for classic operations
the same asset can move through *different* contract wrapper
addresses, so `sep0011_asset` disambiguates.

**Consequence for us:** our unified event decoder must handle
variable topic arity:

- 3 topics for SEP-41 / SAC.
- 4 topics for CAP-67 classic unified events.

Specifically topic 0 is always the symbol `"transfer"` / `"mint"` /
etc; from/to are topics 1/2; classic events have `sep0011_asset`
as topic 3.

## Data field — dual form

Same as SEP-41 v0.4.0+ — `data` can be:

```
1. i128                           // simple
2. map { amount: i128, to_muxed_id: Option<u64|String|BytesN<32>> }
```

Decoder must type-test. Never assume simple form.

## New SC address types

CAP-67 extends `SCAddressType` with three new variants
(`Stellar-contract.x` diff in the CAP):

```
SC_ADDRESS_TYPE_MUXED_ACCOUNT    = 2
SC_ADDRESS_TYPE_CLAIMABLE_BALANCE = 3
SC_ADDRESS_TYPE_LIQUIDITY_POOL   = 4
```

**Implication:** when we decode `from` / `to` addresses in these
events, they can be **claimable balance IDs or liquidity pool IDs**,
not just account or contract addresses. Our address decoder must
dispatch on the `SCAddressType` discriminant; `strkey.Encode()` for
account/contract alone is insufficient.

## Operation → event mapping (verified, spec §3)

### Classic payment-family ops → `transfer`

`Payment`, `CreateAccount`, `AccountMerge`, etc emit:

```
["transfer", from, to, sep0011_asset]  data = amount:i128
```

For XLM the asset is `"XLM"`.
For credit assets it's `"CODE:ISSUER"`.

If `from` is the **issuer** of the asset → emit `mint` instead:

```
["mint", to, sep0011_asset]  data = amount:i128
```

If `to` is the **issuer** → emit `burn` instead.

### `CreateClaimableBalance`

```
["transfer", from, to, sep0011_asset]  data = amount
```

Where `to` is a `SC_ADDRESS_TYPE_CLAIMABLE_BALANCE` — our decoder
must recognise this as a non-account destination.

### `ClaimClaimableBalance`

Symmetrical — `from` is the claimable balance.

### `LiquidityPoolDeposit` — two events per deposit

```
contract: assetA, topics: ["transfer", from, to, sep0011_asset]  data: amount
contract: assetB, topics: ["transfer", from, to, sep0011_asset]  data: amount
```

`to` is `SC_ADDRESS_TYPE_LIQUIDITY_POOL`. Important: this lets us
**track LP reserves movement from event stream alone** post-P23,
without pool-state queries. Pre-P23 we had to read
`LiquidityPoolEntry` changes.

### `LiquidityPoolWithdraw` — symmetrical

### `ManageBuyOffer` / `ManageSellOffer` / `CreatePassiveSellOffer`

**Two events per offer filled** — one per asset leg of the trade:

```
contract: assetA, topics: ["transfer", seller, buyer, sep0011_asset]  data: sold_amount
contract: assetB, topics: ["transfer", buyer, seller, sep0011_asset]  data: bought_amount
```

If `from`/`to` is the issuer of either asset, the corresponding leg
emits `mint`/`burn` instead of `transfer`.

**This is powerful for us**: post-P23, a single subscription to
`"transfer"` events **captures every SDEX trade without parsing
ClaimAtoms**. Pre-P23 ledgers still need the ClaimAtom path (see
[../dexes-amms/sdex.md](../dexes-amms/sdex.md)).

### `PathPayment{StrictSend,Receive}` — emit the same per-hop pattern

Each hop becomes two `transfer` events (or mint/burn at issuer boundaries).

### `Clawback` / `ClawbackClaimableBalance`

```
["clawback", from, sep0011_asset]  data: amount
```

As in SEP-41, clawback emits only one event. No burn/transfer sibling.

### `AllowTrust` / `SetTrustLineFlags` → `set_authorized`

```
["set_authorized", id, sep0011_asset]  data: authorize:bool
```

If the operation revokes authorisation **and** the account held LP
shares, the resulting forced-withdraw claimable balances emit
additional transfer events per asset. The full ordering is specified
in CAP-67 §3.

### `Inflation` → `mint` per winner

Rare now (inflation was disabled network-wide), but event-shaped for
completeness.

## Transaction-level events

New XDR type `TransactionEvent` carries non-operation events at three
stages:

```rust
enum TransactionEventStage {
    BEFORE_ALL_TXS = 0,
    AFTER_TX       = 1,
    AFTER_ALL_TXS  = 2,
}
```

Currently used for **fee events**. `TransactionMetaV4.events<>` holds
these. Our indexer should subscribe if we ever build fee analytics;
not on the pricing critical path.

## New `TransactionMetaV4` (spec §2)

```
struct TransactionMetaV4 {
    ExtensionPoint ext;
    LedgerEntryChanges txChangesBefore;
    OperationMetaV2 operations<>;
    LedgerEntryChanges txChangesAfter;
    SorobanTransactionMetaV2* sorobanMeta;
    TransactionEvent events<>;                  // NEW
    DiagnosticEvent diagnosticEvents<>;         // NEW
}
```

And new `OperationMetaV2` (replaces V1 + adds events):

```
struct OperationMetaV2 {
    ExtensionPoint ext;
    LedgerEntryChanges changes;
    ContractEvent events<>;                     // per-op events
}
```

**For our decoder:** the SDK's `tx.GetTransactionEvents()` helper
abstracts V1/V2/V3/V4 dispatch. We don't touch these types directly
unless we're debugging.

## Architectural consequences for us

1. **Pre-P23 vs post-P23 paths diverge materially.**
   - Pre-P23 (ledger 2 → ~53M-ish): parse operations + effects for
     classic trades (per [../dexes-amms/sdex.md](../dexes-amms/sdex.md)),
     parse Soroban contract events for Soroswap/Aquarius.
   - Post-P23: subscribe to unified `"transfer"` event on every asset
     contract; both classic and Soroban trades show up as transfers
     with the pool/orderbook pseudo-address as destination.

2. **Address-type dispatch is new work.** Our `from`/`to` parser now
   handles 5 variants (account / contract / muxed / claimable balance
   / liquidity pool). Anything operating on strkey-encoded addresses
   must treat the latter two as opaque IDs, not G-keys.

3. **Trade reconstruction from unified events.** Post-P23, a classic
   SDEX trade emits two `transfer` events. To rebuild the trade we
   pair them by `(ledger, tx_hash, op_index)` — same pattern as
   Soroswap swap+sync pairing.

4. **Source-of-truth backfill.** For the since-inception OHLC
   endpoint the RFP requires, we rebuild the full historical trade
   set from ClaimAtom parsing (pre-P23) + unified events (post-P23).
   We never rely on unified events for pre-P23 ledgers — they don't
   exist there.

## `sep0011_asset` — the asset-identity string

CAP-67 uses SEP-11's canonical asset representation:

- `"XLM"` or `"native"` for XLM.
- `"CODE:ISSUER"` for credit assets (e.g. `"USDC:GA5Z…"`).

Our asset-identity parser already handles both; no new code needed
apart from recognising it as a topic-3 string on unified events.

## Open items

- [ ] Decide whether our post-P23 trade path reads unified events or
      continues via ClaimAtoms for consistency. Likely answer:
      **ClaimAtom path for uniformity with pre-P23**; cross-verify
      via unified events as a sanity check. This keeps one code
      path for all of history.
- [ ] Test-decode a real P23 LiquidityPoolDeposit ledger from pubnet
      — confirm the two-transfer-events pattern actually matches
      what's emitted.
- [ ] Design our ledger-range-aware subscription logic: pre-P23
      → ClaimAtom extraction only, post-P23 → unified events
      optionally.

## References

- CAP-67 primary:
  `stellar-protocol/core/cap-0067.md`.
- Related: [sep-41-token-events.md](sep-41-token-events.md),
  [../dexes-amms/sdex.md](../dexes-amms/sdex.md),
  [../protocol-versions.md](../protocol-versions.md).
