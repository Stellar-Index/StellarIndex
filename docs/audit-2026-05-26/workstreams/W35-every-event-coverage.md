# W35 — Granular-coverage mission audit

## Scope

For every Soroban source, enumerate every event the contract
emits on-chain and verify that the decoder's `classify()`/match
chain claims it. This is the audit-level enforcement of the
project-standing policy
([project_every_event_principle](../../../.claude/projects/-Users-ash-code-ratesengine/memory/project_every_event_principle.md))
that we ship NO partial-event decoders.

## Inputs

- `internal/sources/<source>/decode.go` (per-source classify
  switch + decoder branches)
- per-source upstream contract source / `event.rs` / equivalent
  documentation
- `docs/operations/wasm-audits/<source>.md` (per-source WASM
  audit log; W24)
- the rc.80 decoder PRs (Comet #26 / Soroswap-skim #28 /
  Phoenix #27 / Blend #25) — re-verified cold here, NOT trusted
  from the PR description
- `cmd/ratesengine-ops/verify-decoders` subcommand if it exists

## Methodology

For each source, produce one row per (contract, on-chain-event)
in `every-event-coverage.tsv`:

| source | contract | event_topic_0 | event_topic_1 (if applicable) | classified_by_decoder | persisted_to_table | tested | notes |

Columns:

- `source`: canonical source name (matches `internal/sources/<source>/`)
- `contract`: contract id or "any" (for shared-topic-namespace sources)
- `event_topic_0`: the Symbol or String on topic[0]
- `event_topic_1`: when applicable (two-tuple topics)
- `classified_by_decoder`: `yes` / `no` (the smoking gun)
- `persisted_to_table`: target hypertable or "skipped (already
  in trades)" or "skipped (factory)" etc.
- `tested`: `unit` / `integration` / `none`
- `notes`: free text (e.g. "decoder treats as malformed",
  "decoder silently drops", "decoder routes to wrong table")

## Sources in scope

| Source | known event surface (to verify cold) | upstream ref |
| --- | --- | --- |
| soroswap (pair) | swap, sync, deposit, withdraw, skim | Soroswap pair contract `event.rs` |
| soroswap (factory) | new_pair | Soroswap factory `event.rs` |
| soroswap_router | TBD (NEW source — what does the router emit?) | Soroswap router source |
| phoenix (pool) | swap (8-field), provide_liquidity (5-event), withdraw_liquidity (4-5-event) | Phoenix pool `event.rs` (volatile + stableswap) |
| phoenix (stake) | bond (3-event), unbond (3-event) | Phoenix stake `event.rs` |
| aquarius | swap, deposit, withdraw, ? | Aquarius v2 contract |
| comet (POOL namespace) | swap, join_pool, exit_pool, deposit, withdraw | comet-contracts-v1 Stellar port |
| blend (pool money-market) | supply, withdraw, supply_collateral, withdraw_collateral, borrow, repay, flash_loan, gulp, claim, bad_debt, defaulted_debt, reserve_emission_update, gulp_emissions, set_admin, update_pool, queue_set_reserve, cancel_set_reserve, set_reserve, set_status, deploy | blend-contracts-v2 pool `events.rs` |
| blend (auctions, legacy) | new_auction, fill_auction, delete_auction | blend-contracts-v2 |
| reflector (DEX) | price feed updates | Reflector DEX contract |
| reflector (CEX) | price feed updates | Reflector CEX contract |
| reflector (FX) | price feed updates | Reflector FX contract |
| redstone | WritePrices | RedStone Adapter |
| band | (zero events; ContractCallDecoder observes relay/force_relay calls) | Band contract |
| cctp (token-messenger-minter) | deposit_for_burn, mint_and_withdraw | CCTP v2 contracts on Stellar |
| cctp (message-transmitter) | message_sent, message_received | — |
| cctp (forwarder) | (passes events through) | — |
| rozo (payment) | payment, flush | Rozo intent-bridge `event.rs` |
| defindex (vaults) | TBD (NEW source — what do the vaults emit?) | DeFindex sources |
| sep41_supply | transfer, mint, burn, clawback (CAP-67 + pre-CAP-67 shapes) | SEP-41 spec + CAP-67 |
| sdex | classic operations + effects (not Soroban events) | Stellar XDR |

## Per-source check

| # | Check | Method |
| --- | --- | --- |
| W35.X.1 | Enumerate every emitted event by inspecting the contract's `event.rs` (or equivalent) | upstream code read |
| W35.X.2 | For each emitted event, decoder's `classify()` claims it (or the source has documented why the event is intentionally skipped) | grep classify branches |
| W35.X.3 | The decoder Decode path produces a `consumer.Event` (or returns nil with explicit reason) | code trace |
| W35.X.4 | The sink persists the consumer.Event to the right table | type-switch in sink.go |
| W35.X.5 | Migration creates the target table with appropriate columns | migration audit |
| W35.X.6 | Tests cover at least one happy-path event per kind | test inspection |
| W35.X.7 | Tests cover at least one malformed-input case | test inspection |
| W35.X.8 | The matching `<source>-backfill` subcommand (if applicable, W29) filters by EVERY kind the decoder claims | per-subcommand check |

## Special situations

### Two-tuple topics

Some sources put the prefix on topic[0] and the event name on
topic[1]:
- Soroswap (pair contracts): `topic[0]="SoroswapPair"` (String),
  `topic[1]=symbol`
- Soroswap (factory): `topic[0]="SoroswapFactory"` (String),
  `topic[1]=symbol`
- Comet: `topic[0]="POOL"` (Symbol), `topic[1]=symbol`
- Phoenix: `topic[0]=action_symbol` (String) — single-tuple
- CCTP: `topic[0]=symbol`, additional topic[1..3] carry indexed
  fields

The W35 register must reflect the actual topic shape per source.

### Catch-all source: sorobanevents

`internal/sources/sorobanevents/` is the catch-all (ADR-0029).
It does NOT claim a contract — it claims EVERY event. So:

- For sources that ALSO have a per-source decoder, the event is
  double-captured (sorobanevents writes the raw row + the
  per-source decoder writes the structured row).
- For events that NO per-source decoder claims, sorobanevents
  alone captures them. These rows are the SQL-backfill seed
  for future decoders.

The W35 register flags "raw-only-captured" events as opportunities
for future decoders (these aren't gaps in our delivered scope,
but they ARE rows showing platform reach).

### ContractCallDecoder sources

Band emits zero events but is observed via the
`ContractCallDecoder` interface (relay / force_relay calls). W35
must record this separately:

| W35.BAND.1 | Band's relay/force_relay calls land via ContractCallDecoder | dispatcher.go ContractCallDecoder hooks |
| W35.BAND.2 | Decoder reads op args (events.OpArgs / op_args_xdr from soroban_events catch-all) | code path |

### Migration-only flow (no decoder)

Some sources are observed via `LedgerEntryChangeDecoder` (account
entries, trustlines, etc.) and produce no Soroban events. W35
records the ledger-entry-change surface per source, similarly:

| W35.ACCOUNTS.1 | AccountEntry mutation → `internal/sources/accounts/` | dispatcher LedgerEntryChangeDecoder hook |

## Output artifact

`inventory/every-event-coverage.tsv`. Generated from manual
verification + grep + decoder code reads. Closure: every (source,
event) tuple has terminal status.

## Closure criteria

Every row in `every-event-coverage.tsv` is one of:

- `classified_by_decoder=yes` + tests + persistence → `done`
- `classified_by_decoder=no` + documented reason → `note`
- `classified_by_decoder=no` + no reason → finding (severity:
  `high` if the event carries decisional data; `medium` if it's
  pure-info; `low` if it's deprecated)

Findings on:
- any decoder that claims a contract but silently drops events
- any factory event that's documented but unclaimed
- any source that has matching `<source>-backfill` subcommand
  whose filter list doesn't match the decoder's claim list
