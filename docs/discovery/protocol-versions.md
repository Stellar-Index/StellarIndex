# Protocol-version / epoch normalization

**Status:** ✅ Research complete. Architectural concern **smaller than we
feared** — the SDK handles version dispatch for us. We still enumerate
the upgrade history here so we know the feature-availability timeline.

**Verified against:**
- `go-stellar-sdk/xdr/xdr_generated.go:19180-20858` (union structures)
- `go-stellar-sdk/xdr/xdr_generated.go:37430-37900` (ClaimAtom variants)
- `stellar-docs/docs/networks/software-versions.mdx`
- `withObsrvr/cdp-pipeline-workflow/CLAUDE.md` (SDK helper guidance)
- `withObsrvr/stellar-extract/trades.go` + `effects.go`

## The two critical questions — answered

### Q1. Does Galexie upcast ledgers to the current XDR schema, or preserve native-epoch XDR?

**Answer: preserves native-epoch.** Confirmed from the XDR union
structure.

`LedgerCloseMeta` is an XDR union with three arms:

```
union LedgerCloseMeta switch (int v) {
    case 0: LedgerCloseMetaV0 v0;     // pre-Soroban era
    case 1: LedgerCloseMetaV1 v1;     // early Soroban (Protocol 20)
    case 2: LedgerCloseMetaV2 v2;     // current
};
```

`TransactionMeta` is a union with **five** arms (V0 through V4).

Galexie writes the raw XDR exactly as stellar-core emits it. The
discriminant `V` tells the consumer which arm to read. Pre-Soroban
ledgers are `v0`, post-Soroban `v1`/`v2`.

### Q2. Do we need per-epoch decoders in our code?

**Answer: mostly no.** The SDK handles dispatch.

- At the ledger level, `ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)`
  works across all three `LedgerCloseMeta` variants transparently.
- At the transaction level, **SDK helper methods abstract the
  `TransactionMeta` V1/V2/V3/V4 split**. From
  `cdp-pipeline-workflow/CLAUDE.md` verbatim:

  > "When processing Stellar transactions, **ALWAYS use SDK helper
  > methods instead of accessing metadata directly.** The SDK abstracts
  > protocol version differences (V1, V2, V3, V4) automatically.
  >
  > `tx.GetTransactionEvents()` NOT `tx.UnsafeMeta.V3.SorobanMeta.Events`
  > `tx.Result.Successful()` NOT manual result code checks
  > `op.Body.MustPayment()` etc. NOT manual field access
  > `strkey.Encode()` NOT base64 or manual encoding"

- At the claim-atom level, the three `ClaimAtomType` variants (V0,
  OrderBook, LiquidityPool) **do require explicit handling in our
  code** — there's no SDK helper that unifies them. `stellar-extract`
  already does this correctly; we adopt that pattern. See
  [dexes-amms/sdex.md](dexes-amms/sdex.md).

So the places where our code must be version-aware:

1. **ClaimAtom variants** — three variants, explicit `switch
   claim.Type` required. Already handled by `stellar-extract/trades.go`
   and `effects.go`.
2. **Feature availability gates** — e.g. don't try to extract
   Soroban contract events from pre-P20 ledgers. The SDK will return
   empty / `nil` when features aren't present; we need to treat that
   as normal, not as error.

Everything else (transaction iteration, operation body access, event
extraction, strkey encoding) is SDK-abstracted.

## Pubnet upgrade timeline (verified)

From `stellar-docs/docs/networks/software-versions.mdx`. Ledgers
numbers are approximate until we query live:

| Protocol | Name         | Pubnet activation | Key features                                         |
| -------- | ------------ | ----------------- | ---------------------------------------------------- |
| 25       | X-Ray        | 2026-01-22        | BN254 curve ops (CAP-79), Poseidon/Poseidon2 hashes (CAP-75) |
| 24       | —            | 2025-10-22        | Stability upgrade after Whisk; state-archival fixes (CAP-76) |
| 23       | Whisk        | 2025-09-03        | **Unified Events (CAP-67)**, State Archival (CAP-62, CAP-66) |
| 22       | —            | 2024-12-05        | Constructor support (CAP-58), BLS12-381 host functions (CAP-59) |
| 21       | —            | 2024-06-18        | (see release notes)                                  |
| 20       | Soroban Ph2  | 2024-03-19        | Resource limits / fees Phase 2                       |
| 20       | **Soroban Ph1 (Mainnet)** | **2024-02-05** | **Soroban introduced.** Contracts, SAC, contract events. Before this → no contract activity at all. |
| 19       | —            | 2023              | `PathPaymentStrictSend` existed; various fee and SCP changes |
| 18       | —            | 2022              | **Liquidity pools** (classic AMM) introduced — `ClaimAtomTypeLiquidityPool` appears. |
| 17       | —            | 2021              | Muxed accounts (CAP-27) activated. Affects `SellerId` parsing for some variants. |
| <17      | —            | 2015–2021         | `ClaimAtomTypeV0` used (raw Ed25519 public key instead of `AccountId`). |

**Protocol 23's "Unified Events" (CAP-67)** is particularly relevant
to us. It unified the previously scattered event model — **after P23,
events include asset transfer diagnostic events** on classic
operations, not just Soroban. This is the source of the SDK's
`tx.GetTransactionEvents()` abstraction. Pre-P23 that method returns
Soroban events only; post-P23 it returns unified events for classic
ops too. Our event consumers need to account for this: if we want
classic-asset transfer events pre-P23, we parse operations, not
events.

## Feature-availability boundaries that matter for pricing

### Pre-P18 (before ~2022)

- No liquidity pools exist.
- `ClaimAtomTypeLiquidityPool` (type 2) is not a valid variant.
- No `LiquidityPoolEntry` ledger entries.
- PathPayment only routes through orderbook.

### P18 – P19

- Liquidity pools exist (classic constant-product, 30 bps).
- `ClaimAtomTypeLiquidityPool` valid.
- All three ClaimAtom variants in play (V0 historical, OrderBook
  modern, LiquidityPool new).

### Pre-P20 (before 2024-02-05)

- **No Soroban contracts.** No Soroswap, Aquarius, Blend, Reflector,
  Redstone, Band, DIA.
- No contract events.
- No SAC-wrapped classic assets.
- Nothing in our "Soroban DEX" pricing path applies.

### P20 – P22

- Soroban live; contracts can be deployed.
- Soroswap factory deploys early in this era (verify ledger).
- Contract event model exists but not yet unified.

### P23+ (Whisk, 2025-09-03)

- **Unified Events (CAP-67)** — classic-asset operations also emit
  events. Our event-driven pipeline becomes more complete for classic
  token transfers.
- **State Archival** (CAP-62, CAP-66) — ledger entries can be
  archived after a TTL. Our pipeline must handle `archived` /
  `restored` key events (captured in stellar-extract as
  `EvictedKeys` / `RestoredKeys`).

## Practical routing rules for our extractor

1. **Never hard-code a protocol version** in extraction logic. Always
   call SDK helpers.
2. **Gracefully handle missing features.** `tx.GetTransactionEvents()`
   on a pre-P20 tx returns an empty slice — treat as success, not
   error. Similarly `operation.Body.MustManageBuyOfferOp()` requires
   checking `op.Body.Type` first.
3. **Switch on `ClaimAtomType`**, never assume the arm. Our trade
   consolidator handles V0 / OrderBook / LiquidityPool uniformly.
4. **Respect feature gates in schema design.** Our `pool_reserves`
   table gets its first row at the P18 activation ledger, not before.
   Our `contract_events` table gets its first row at the P20
   activation ledger. Etc. Nulls / missing rows at older ledgers are
   expected.
5. **Route per-file by `protocol-version` metadata** on Galexie
   objects (from
   `go-stellar-sdk/support/datastore/object_metadata.go`) for
   efficient "which decoder paths to enable" decisions — without
   opening the file. Useful for parallel batch reprocessing.

## What we will test

When we build the canonical-trade consolidator:

1. **Fixture per ClaimAtom variant.** V0 (pre-P17), OrderBook (P17+),
   LiquidityPool (P18+). One ledger each, replayed through our
   consolidator, yields the expected `Trade` rows.
2. **Fixture per trade-producing op type.** `ManageSellOffer`,
   `ManageBuyOffer`, `CreatePassiveSellOffer`,
   `PathPaymentStrictSend`, `PathPaymentStrictReceive`. Each produces
   at least one claim atom in the fixture.
3. **Fixture at each protocol boundary.** One ledger just before P18
   activation + one just after. One just before P20 + one just after.
   Verify the decoder handles the transition without panics.
4. **Fixture from P23 forward** showing unified-event emission on a
   classic payment. Confirm our event consumer picks it up.

## Open items

- [ ] Query pubnet to confirm the exact first-ledger number for each
      protocol activation. Store in a `PROTOCOL_UPGRADES` constant in
      our code for tests and monitoring.
- [ ] Read CAP-67 in full to understand the unified-events schema
      changes. Current understanding is limited to "it exists from
      P23" — the detailed event shape needs documentation.
- [ ] Build the fixture set above in CI. Each fixture replayed
      through our consolidator should be committed as a JSON golden
      file.
- [ ] Confirm the earliest ledger our Galexie data lake covers —
      ideally genesis (ledger 2), depending on whether we can mirror
      SDF's full historical bucket. See
      [data-sources/stellar-data-lakes.md](data-sources/stellar-data-lakes.md).

## Summary

The protocol-version concern is mostly an SDK dispatch concern, which
is solved. Our responsibilities are:

- Use SDK helpers everywhere.
- Explicit `switch` on `ClaimAtomType` in our trade consolidator.
- Feature gates in schema + extractor (no pre-P18 LP rows, no pre-P20
  contract events, etc.).
- Fixtures at every relevant protocol boundary in CI.

Everything else the SDK handles.

## References

- Software versions table: `stellar-docs/docs/networks/software-versions.mdx`
- LedgerCloseMeta + TransactionMeta unions:
  `go-stellar-sdk/xdr/xdr_generated.go:19180-20858`
- ClaimAtomType variants: `go-stellar-sdk/xdr/xdr_generated.go:37430-37900`
- Galexie per-file metadata keys: see
  [data-sources/galexie.md](data-sources/galexie.md) and
  [data-sources/composable-data-platform.md](data-sources/composable-data-platform.md)
- SEP-40 oracle spec (relevant for Reflector versioning):
  [oracles/reflector.md](oracles/reflector.md)
- CAPs referenced above:
  - CAP-67 (Unified Events):
    <https://github.com/stellar/stellar-protocol/blob/master/core/cap-0067.md>
  - CAP-58 (Constructors):
    <https://github.com/stellar/stellar-protocol/blob/master/core/cap-0058.md>
  - CAP-62 / CAP-66 (State Archival):
    <https://github.com/stellar/stellar-protocol/blob/master/core/cap-0062.md>
    <https://github.com/stellar/stellar-protocol/blob/master/core/cap-0066.md>
- Related: [decisions.md](decisions.md) (i128 invariant applies
  uniformly across all protocol eras for Soroban values; classic
  amounts remain int64).
