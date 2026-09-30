# defindex source

Decoder for paltalabs' [DeFindex](https://github.com/paltalabs/defindex)
protocol on Stellar mainnet. It covers three contract layers — the
Blend autocompound **strategy** contracts (WASM `11329c24…988`), the
**vault** wrappers users deposit into, and the vault **factory** — so
`defindex` names the whole protocol, not the strategy layer alone.

## What it decodes (verified on-chain)

| topic | body | output |
|---|---|---|
| `("BlendStrategy","deposit"\|"withdraw")` | `Map{ from: Address, amount: i128 }` | `Event{StrategyFlow}` |
| `("BlendStrategy","harvest")` | `Map{ from, amount: i128, price_per_share: i128 }` | `Event{StrategyFlow}`, direction `harvest` |
| `("DeFindexVault","deposit")` | `Map{ depositor, amounts: Vec<i128>, df_tokens_minted: i128 }` | `VaultEvent{VaultFlow}` |
| `("DeFindexVault","withdraw")` | `Map{ withdrawer, amounts_withdrawn: Vec<i128>, df_tokens_burned: i128 }` | `VaultEvent{VaultFlow}` |
| `("DeFindexVault","dfees")` | `Map{ distributed_fees: Vec[(token, amount i128)] }` | one `DFeesEvent` per entry (see below) |
| `("DeFindexVault", rebalance\|rescue\|paused\|unpaused\|nreceiver\|nmanager\|nemanager\|rbmanager\|n_wasm)` | — | recognised, not modelled |
| `("DeFindexFactory","create"\|"n_fee")` | — | recognised, body never decoded (see below) |

- `topic[0]` is an `ScvString` on every layer (each prefix exceeds
  the 9-char `symbol_short!` cap, same pattern as `"SoroswapPair"`).
- The strategy layer's `from` is the vault contract moving capital,
  not the end user; the vault layer's `depositor` / `withdrawer` is
  the end user (`VaultFlow.User`).
- Fields are read by map name, never by position; every amount is an
  `i128` carried as `canonical.Amount` (never truncated — ADR-0003).
- `harvest` is strategy yield realised into the vault, not a user
  flow: position sums exclude it, NAV must include it.

These are **flow-attribution** events, not price discovery — they move
capital at NAV and never set a market price. Registered
`Class: ClassRouter`; never a VWAP contributor.

## Dispatch

Standard event-based `dispatcher.Decoder`. `Matches` gates on
**contract identity** (ADR-0035): a strategy or vault topic matches
only when the emitter is registered (`MainnetStrategies` /
`MainnetVaults` plus the `protocol_contracts` warm), and a factory
topic only when the emitter is one of `MainnetFactories`. An
unregistered emitter fail-closes into an ADR-0033 recognition gap.

## Files

```
events.go              — source name, topic prefixes/symbols, StrategyFlow / VaultFlow / DFee and their Event wrappers, curated contract sets
decode.go              — classify / classifyVault / classifyFactory, decodeFlow / decodeVaultFlow / decodeDFees, DecodeRebalanceMethod
dispatcher_adapter.go  — implements dispatcher.Decoder (topic-matched + contract-identity gated)
README.md              — this file
```

`defindex` is a projected source (ADR-0031/0032): `internal/projector`
is its only writer, through the `defindex.Event` / `VaultEvent` /
`DFeesEvent` cases in `internal/pipeline/sink.go`. Flows land in
`defindex_flows` (migration 0050) at the `strategy` and `vault`
layers; `dfees` entries land in `defindex_fees` (migration 0146).

## Current scope (shipped)

- Decode `("BlendStrategy","deposit"|"withdraw")` across all
  registered emitters → `StrategyFlow`, plus the user-facing vault
  flows → `VaultFlow`; both persist to `defindex_flows`.
- `BackfillSafe` is `true` (audited 2026-05-19 against the real
  deployed hash `11329c24…988`; see
  `docs/operations/wasm-audits/defindex.md`).
- **Factory `create` bodies are recognised but NOT trusted for
  registry fan-out (task #34, W8 recon 6c).** A
  `("DeFindexFactory","create")` event is recognised (so the
  drop-counter doesn't file it as an unmatched topic) and drops
  cleanly with no output; its body is **not decoded**. The DeFindex
  factory is PERMISSIONLESS — anyone can create a vault, and the
  create body's `assets[].strategies[].address` fields are
  attacker-controlled — so a canonical-factory emitter does not vouch
  for the addresses it names. The earlier ROADMAP #7 fan-out (which
  Seeded every named `address` into the registry) was a
  permissionless-poisoning vector: an attacker could register
  arbitrary contracts as "strategies" merely by naming them, whereupon
  their subsequent `("BlendStrategy",…)` events decoded as recognised
  DeFindex flows and contaminated flow/TVL stats. It has been removed.
- **Both layers now register the same way: curated set + operator
  seed only.** No execution-corroboration signal (the dispatcher's
  `ExecutionCorroborated`) is available on this event decode path, so
  the decoder fails closed. `MainnetStrategies` (evidence-verified,
  16/16 as of 2026-07-10) and `MainnetVaults` are the always-seeded
  trust roots; the `protocol_contracts` warm is the operator seam for
  admitting a newly VERIFIED strategy or vault. A new, un-verified
  strategy fail-closes into an ADR-0033 recognition gap — exactly the
  posture vaults already had (no create body carries the vault's own
  address either).

## Settled follow-ups

None of the follow-ups once listed here is open work.

1. **`trades.routed_via` tagging** — not applicable. DeFindex vaults
   hold persistent capital and do not take part in per-tx
   `routed_via` tagging; their state lives in `defindex_flows`, not
   `trades` (migration 0072). `internal/pipeline/routedvia.go` tags
   Soroswap router legs only.
2. **Exposure ticker** — dropped. The `aggregator_exposures` table it
   would have written was never wired and migration 0152 removed it.
3. **`harvest`** — modelled as `DirectionHarvest` in `defindex_flows`
   (migration 0138 admits the direction).
4. **End-user attribution** — served by the vault layer, which names
   the end user directly: `VaultFlow.User` is the `defindex_flows`
   vault-layer `actor` that `DefindexVaultSharesByUser`
   (`internal/storage/timescale/positions.go`) folds. No same-tx
   correlation with the strategy layer is needed.
5. **Source rename** — not pursued. The source decodes the vault,
   factory and fee layers as well as `BlendStrategy`, so `defindex`
   is the accurate registry key.

## `dfees` — MODELLED (W5.2, 2026-08)

The vault-layer `("DeFindexVault","dfees")` protocol-fee-distribution
event graduated from recognised-only to fully modelled once its body
shape was captured and proven from live r1-lake blobs (decoded with
`internal/scval` — the do-not-invent unblock, same path `harvest`
took):

    Map{ distributed_fees: Vec[ (token Address<contract>, amount i128) ] }

PER-ASSET (fee token contracts — captured samples include USDC's SAC),
NOT per-recipient; 0..N entries, and an EMPTY Vec is a real observed
shape (a distribution ran with nothing to distribute → zero events, no
error). Lake facts at capture: 12,785 events on 27 vault contracts,
ledgers 60,903,337 → tip, still firing live — every sample in the SAME
op as the vault deposit/withdraw flow (op_index 0, event_index 5),
which is why dfees lands in its own `defindex_fees` table (migration
0146, `fee_index` PK discriminator for the per-entry fan-out) instead
of a third `defindex_flows` layer. The decoder (`decodeDFees`) emits
ONE `DFeesEvent` per Vec entry so the ADR-0033 projection reconcile
counts 1:1; kind `defindex.vault.dfees`. Historical fill:
`stellarindex-ops projector-replay -source defindex` (ADR-0034).

## `n_wasm` — HANDLED as classify-only (ROADMAP #89, 2026-07-10)

A read-only lake topic census against the gated vault set found 2
real `n_wasm` events (vault-layer topic[1]) that `classifyVault`
didn't recognize — alongside the 11 topics it does (`deposit`,
`withdraw`, `rescue`, `paused`, `unpaused`, `nreceiver`, `nmanager`,
`nemanager`, `rbmanager`, `dfees`, `rebalance`). `n_wasm` is now
classified (`EventNWasm`, `events.go`) the same way as the other 9
admin topics — recognised so `classifyVault`'s drop-counter doesn't
file it as "unmatched topic" (EVERY-event policy), no flow modelled
(the name suggests a WASM-upgrade announcement — "new wasm" — matching
the `n_receiver`/`n_manager`/`n_emanager` "new-X" naming convention
among the vault's other admin topics, but that reading is inferred
from the name, not from a captured body).

A real-lake-bytes body sample was NOT pulled — three separate
ClickHouse queries against the raw lake (`stellar.contract_events`,
233M+ rows) each timed out past 400s server-side: a contract-scoped
query (85 gated vault addresses via `contract_id IN (…)`), a
ledger-range-scoped query (`ledger_seq >= 55000000`), and a
topic-only query (`topic_count = 2 AND topics_xdr[2] = …`) — none of
the predicates besides `contract_id` (whose bloom-filter index only
covers newer parts per the schema comment) let ClickHouse skip
granules on this table, and 2 real occurrences in 233M+ rows isn't
worth a heavier operator-run query. Classification is verified (the
topic's symbol encoding was computed via `scval.MustEncodeSymbol`,
the same mechanism the whole package's topic matching relies on) —
`decode_test.go`'s `TestClassifyVault_depositWithdraw/vault n_wasm`
case. A future census re-run (or an operator-run heavy query via
`run-heavy-job.sh`) can pull the real body if a decoder is ever
warranted.

The same pass also saw ambiguous rows with an empty decoded topic[0]
but a populated topic[1] — likely a `contract_events_daily`
2-topic-only census artifact, not asserted as a distinct gap; see
`internal/sources/phoenix/README.md`'s rewards-topics section for
the same caveat on a sibling source.

## Sources

- Event shapes: **real mainnet LCM**, captured via
  `stellarindex-ops scan-soroban-events` (2026-05-19).
- Deployed WASM: `11329c2469455f5a3815af1383c0cdddb69215b1668a17ef097516cde85da988`
  (Blend strategy code; walk-confirmed single hash, zero upgrades).
- WASM audit: `docs/operations/wasm-audits/defindex.md`.
- Factory `create` body shape (the `assets[].strategies[].address`
  fan-out field): **real lake bytes**, ClickHouse HTTP `:8123` against
  r1's certified raw lake (2026-07-10), contract-scoped to all 3
  create-emitting `DeFindexFactory` instances, cross-ledger
  (55,484,403 → 57,147,588) to confirm the schema is stable across the
  full factory-era history. Golden test constants:
  `decode_test.go`'s `createBodyTwoStrategies` /
  `createBodyZeroStrategies` / `createBodyEarliestFactory`.
