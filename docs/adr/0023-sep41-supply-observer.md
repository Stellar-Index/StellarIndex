---
adr: 0023
title: SEP-41 supply observer — mint / burn / clawback event-stream tracking
status: Accepted
date: 2026-04-30
supersedes: []
superseded_by: null
---

# ADR-0023: SEP-41 supply observer — mint / burn / clawback event-stream tracking

## Context

[ADR-0011](0011-supply-algorithm.md) Algorithm 3 derives SEP-41 token supply from a running event sum, `total = Σ mint − Σ burn − Σ clawback` and `circulating = total − admin_balance`. It accumulates events rather than reading state, and the contract's total is not stored anywhere; discovery records contract ids but does not aggregate amounts. Amounts are i128 (ADR-0003).

## Decision

An event-stream observer, `internal/sources/sep41_supply`, writes mint, burn and clawback events to `sep41_supply_events` (migration 0015), and `supply.StorageSEP41SupplyReader` feeds the real `SEP41Computer` (`internal/supply/sep41.go`).

- **Why events, not ledger entries.** Summing holder balances needs a generic SEP-41 `ContractData` observer that does not exist, is expensive even for SAC-backed tokens, and the SEP-41 mint/burn/clawback events are the audit trail. The event sum works for classic-SAC and pure SEP-41 tokens alike.
- **Match.** `contract_id ∈ watched set AND topic[0] ∈ {mint, burn, clawback}`, reusing the discovery sniffer's `classifySymbol`. Bodies are `i128`:

| Event | Topic shape |
|---|---|
| `mint` | `["mint", admin, to]` |
| `burn` | `["burn", from]` |
| `clawback` | `["clawback", admin, from]` |

- **Table.** Key `(contract_id, ledger, tx_hash, op_index, observed_at)`; `event_kind` constrained to the three kinds; `amount NUMERIC CHECK (amount >= 0)`, direction carried by the kind (mint adds, burn and clawback subtract); nullable `counterparty` (mint: the recipient; burn and clawback: the holder reduced). Hypertable on `observed_at`, 7-day chunks.
- **Storage methods.** `InsertSEP41SupplyEvent` is idempotent on the key; `SEP41NetMintAtOrBefore(contract, ledger)` returns `Σ mint − Σ(burn + clawback)`.
- **Reader.** `total = net mint`; `circulating = total − admin balance − Σ locked`, the admin and locked holders read from `sac_balance_observations`.
- **Watched set.** `[supply] watched_sep41_contracts` (C-strkeys), keyed by the bare contract id in `supply.AssetKey`; one `supply.Refresher` per contract in the aggregator (`buildSEP41Refreshers`), with the shared `stellarindex_aggregator_supply_refresh_total{outcome}` counter.
- **Serving fallback.** `/v1/assets/{id}` and `/v1/assets/{id}/supply` fall back to the lake-derived supply, `Σ mint − Σ burn − Σ clawback` over the certified ClickHouse lake's `supply_flows` (`supply_basis: "sep41_lake_flows"`), so every SEP-41 token serves a complete-history total without curation. The watched-set path is the precise source where admin and locked-set exclusion matter.
- **Transfers are ignored.** They move ownership, not supply.

## Invariant

- SEP-41 supply is `Σ mint − Σ burn − Σ clawback`; `transfer` never enters it. Enforced by the `internal/supply` SEP-41 reader and computer tests.
- Event amounts are NUMERIC, non-negative, never truncated (ADR-0003); the kind discriminates direction.
- The decoder matches only `mint`, `burn` and `clawback` on a watched contract, so it never claims a topic another decoder owns (first-match-wins).
- `sep41_supply_events` has one writer, the projector (ADR-0031/0032), so catch-up is `projector-replay`, not a second path.

## Consequences

- One hypertable with low row volume (a handful of events per contract per day) while the watched set stays bounded.
- Three-domain supply coverage closes ADR-0011.
- Admin and locked-set exclusion is operator policy and needs the watched-set path; the lake-derived total does not apply it.

## Evidence

- Migration 0015; `internal/sources/sep41_supply`; `internal/supply/storage_sep41_reader.go`, `internal/supply/sep41.go`; `internal/projector/registry.go` (`sep41_supply` case).
- ADR-0003, ADR-0011, ADR-0021, ADR-0022.
