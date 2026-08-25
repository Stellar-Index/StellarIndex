# defindex source

Decoder for the **Blend autocompound strategy** contracts that back
paltalabs' [DeFindex](https://github.com/paltalabs/defindex) vaults
on Stellar mainnet.

> **Corrected 2026-05-19.** This package previously targeted a
> fictional `("DeFindexVault", …){depositor, amounts:Vec<i128>,
> df_tokens_minted}` schema taken from paltalabs/defindex tag
> `1.0.0`. **Mainnet never deployed that.** The contract addresses
> we watch run Blend *strategy* code (deployed WASM `11329c24…988`)
> and emit a much simpler schema, verified from real on-chain LCM
> via `stellarindex-ops scan-soroban-events`. See
> `docs/operations/wasm-audits/defindex.md` "Audit result" for the
> full evidence trail (#28).

## What it decodes (verified on-chain)

| topic | body | example |
|---|---|---|
| `("BlendStrategy","deposit")` | `ScvMap{ from: Address, amount: i128 }` | L57,056,389 |
| `("BlendStrategy","withdraw")` | `ScvMap{ from: Address, amount: i128 }` | recent live |

- `topic[0]` is `ScvString("BlendStrategy")` (13 chars > the 9-char
  `symbol_short!` cap → String, same pattern as `"SoroswapPair"`).
- `from` is the caller moving capital — usually the vault/router
  **contract** (a C-strkey), occasionally a plain account
  (G-strkey). `scval.AsAddressStrkey` renders both.
- `amount` is the underlying-asset delta (`i128`, never truncated —
  ADR-0003).

These are **flow-attribution** events, not price discovery — a
strategy deposit/withdraw moves capital at NAV, it doesn't set a
market price. Registered `Class: ClassRouter`; never a VWAP
contributor.

## Why a separate decoder

Standard event-based dispatcher (`Decoder` interface, like
`soroswap` / `aquarius` / `comet`). Dispatch is **by topic**: any
contract emitting the `("BlendStrategy",deposit|withdraw)` topic is
matched — the comet/aquarius shared-emitter topology — so we cover
*every* Blend autocompound strategy instance, not a hand-curated
set. (The previous revision filtered on a mislabeled 3-contract
"vault" set.)

## Files

```
events.go              — source name, topic prefix bytes, event symbols, StrategyFlow
decode.go              — classify() + decodeFlow() → StrategyFlow (factory `create` bodies are recognised but NOT decoded/trusted — see below)
dispatcher_adapter.go  — implements dispatcher.Decoder (topic-matched + contract-identity gated)
README.md              — this file
```

Persistence is owned by `internal/pipeline/sink.go`, which type-
switches on the dispatched `Event` / `VaultEvent` and writes
`defindex_flows` (migration 0050) at the `strategy` and `vault`
layers.

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

## Phase B follow-ups

1. **`trades.routed_via` tagging.** When the same tx contains a
   Blend (`("Pool","supply")`) or Soroswap leg, tag those trades
   with the strategy attribution.
2. **Exposure ticker.** Periodic worker writing
   `aggregator_exposures` rows from per-strategy on-chain state.
3. **`harvest` / keeper-admin events.** Decode the autocompound
   `harvest` event (yield realisation) for APY attribution.
4. **End-user attribution.** When `from` is a vault contract,
   correlate with the same-tx vault event to recover the end-user.
5. **Source rename.** `defindex` → e.g. `blend-strategy` (more
   honest; distinct from the existing `blend` pool source). A
   product-taxonomy decision deferred so registry / genesis /
   status-page keys stay stable for now.

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
