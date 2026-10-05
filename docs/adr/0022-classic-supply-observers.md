---
adr: 0022
title: Classic-supply observers — Trustline / ClaimableBalance / LiquidityPool / ContractData entry tracking
status: Accepted
date: 2026-04-30
supersedes: []
superseded_by: null
---

# ADR-0022: Classic-supply observers — Trustline / ClaimableBalance / LiquidityPool / ContractData entry tracking

## Context

[ADR-0011](0011-supply-algorithm.md) Algorithm 2 derives classic credit-asset supply as `total = Σ trustline + Σ claimable + Σ LP-reserve + Σ SAC-wrapped` and `circulating = total − issuer_balance − Σ locked-set`. The algorithm is `internal/supply/classic.go::ClassicComputer`; it needs a storage-side reader that returns the four component sums per `(asset, ledger)`, and [ADR-0021](0021-account-entry-observer.md) set the observer pattern.

## Decision

Four observer + storage + reader stacks under `internal/sources/`, each mirroring the AccountEntry pattern on the `LedgerEntryChangeDecoder` hook, composed into one `supply.StorageClassicSupplyReader` that answers `ClassicSupplyReader` from all four tables.

| Component | XDR entry type | Package | Hypertable (migration) | Identity key |
|---|---|---|---|---|
| Trustline | `Trustline` | `trustlines` | `trustline_observations` (0011) | `(account_id, asset_key)` |
| Claimable | `ClaimableBalance` | `claimable_balances` | `claimable_observations` (0012) | `claimable_id` |
| LP reserve | `LiquidityPool` | `liquidity_pools` | `lp_reserve_observations` (0013) | `(pool_id, asset_key)` |
| SAC-wrapped | `ContractData` | `sac_balances` | `sac_balance_observations` (0014) | `(contract_id, holder)` |

- **Shared table shape.** Hypertable on `observed_at`, 7-day chunks; primary key is the identity key plus `(ledger, observed_at)`; `balance_stroops NUMERIC NOT NULL` (ADR-0003); `is_removal BOOLEAN NOT NULL DEFAULT false` for the Removed variant; `ingested_at` for ops trace.
- **Match** filters on the `LedgerEntryType` discriminant and the entry's asset against the watched set.
- **Watched set, not global.** `[supply] watched_classic_assets` lists classic assets (`CODE-GISSUER`) that are tracked; config load rejects anything that is not a valid classic asset and rejects SAC C-strkeys, which track under the SAC observer's own set. Network-wide tracking (about 100M trustlines) needs its own ADR and storage strategy.
- **One reader seam.** `StorageClassicSupplyReader` (`internal/supply/storage_classic_reader.go`) takes a single `ClassicSupplyStore` interface satisfied by `*timescale.Store`, fans out the component queries, subtracts the issuer balance (from `account_observations`) and locked set, and returns an error on any component failure: a partial sum is never published.
- **Aggregator.** `buildClassicRefreshers` in `cmd/stellarindex-aggregator/main.go` runs one refresher per watched classic asset alongside the XLM one.
- **Broad-coverage complement.** `clickhouse.ClassicCirculatingSupply` derives trustline-sum circulating supply for every classic asset from the lake; it undercounts claimable and LP components. The watched-set pipeline is the precise source.

## Invariant

- A classic supply figure is the four-component sum less issuer and locked balances, or an error: never a partial sum. Enforced by the `StorageClassicSupplyReader` tests and `internal/supply/classic_test.go` (locked-set application, max-supply override, negative-component rejection).
- Every component amount is NUMERIC, never truncated (ADR-0003).
- The observers track only the operator-watched set; widening that is a new ADR.

## Consequences

- Four hypertables, roughly ten times the volume of `account_observations` even watched-set restricted, because trustlines proliferate per asset; compression (`compress_segmentby` on `account_id` / `asset_key`) was to be validated at deploy.
- `/v1/assets/{id}` for watched classic assets serves total, circulating and max through the same fields path as XLM.
- Unwatched classic assets get the lake-derived approximation, not the exact figure.

## Evidence

- Migrations 0011-0014; `internal/sources/{trustlines,claimable_balances,liquidity_pools,sac_balances}`; `internal/supply/storage_classic_reader.go`; `cmd/stellarindex-aggregator/main.go::buildClassicRefreshers`.
- ADR-0003, ADR-0011, ADR-0021.
