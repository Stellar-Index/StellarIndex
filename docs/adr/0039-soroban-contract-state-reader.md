---
adr: 0039
title: Soroban contract current-state reader — read-time decode from the lake
status: Accepted
date: 2026-06-18
supersedes: []
superseded_by: null
---

# ADR-0039: Soroban contract current-state reader

## Context

Event-derived analytics give flow but not current contract state. Blend's per-pool TVL, utilisation and APY live in reserve storage, not in any event, so `/v1/lending/pools` could only serve a window net-flow proxy.
The lake captures every `contract_data` entry (`key_xdr` and `entry_xdr`) in `stellar.ledger_entry_changes` (ADR-0038).

## Decision

Add a read path that, for a known contract and storage key, fetches the latest `contract_data` entry from the lake and decodes it with a per-protocol decoder.

1. **Read-time, not materialised.** No new worker, table or backfill. The reader builds the exact `key_xdr` (as `wasm_lake_reader.instanceKeyXDR` does) and probes the lake's current-state projection `ledger_entries_current` on its `(entry_type, key_xdr)` sort key. It does not scan `ledger_entry_changes`.
2. **Decoders live with the source.** Blend's `internal/sources/blend/storage.go` decodes `ReserveData`, `ReserveConfig` and `PoolConfig` by field name (`scval.MapField`). The interest-rate model is ported from the contract's `interest.rs` and `reserve.rs` into `internal/sources/blend/interest.go` and checked in `interest_test.go` against vectors from the contract's own tests.
3. **Current-state figures.** TVL (supply and borrow underlying times price), utilisation, and borrow and supply APY are computed from certified state. The backstop take rate comes from the pool's `PoolConfig`.
4. **Honest provenance.** A figure from contract storage is labelled current-state. The event-derived window proxy (`net_supplied_30d`, `net_borrowed_30d`) stays separate and is never blended with it.

## Invariant

- Current-state fields and the window net-flow proxy are distinct fields. `internal/api/v1/lending_reserve_decimals_test.go` guards that a reserve with unknown decimals is excluded from `tvl_usd` and the total is flagged `lower_bound`.
- A protocol's storage layout is decoded in that protocol's source package, by field name and never by position.
- Fixed-point rounding matches the contract: `internal/sources/blend/interest.go` uses ceil or floor exactly where `soroban-fixed-point-math` does.
- Money in the reader and the rate model is `*big.Int` or `*big.Rat`, never `float64` (AGENTS.md, ADR-0003).

## Consequences

- Blend gains current-state TVL, utilisation and APY without a backfill. The pattern generalises to any protocol whose state is wanted: build the key, probe the lake, decode.
- Coverage is the live contract-storage capture window. A reserve untouched since capture began is not found. Historical TVL series would need a contract-state backfill, which is deferred.
- A pool with many reserves fans out one lookup per reserve. Results are cached, and a materialised projection is the escape hatch.

## Evidence

`internal/storage/clickhouse/blend_pool_state_reader.go`, `internal/sources/blend/storage.go`, `internal/sources/blend/interest.go`, `internal/sources/blend/interest_test.go`, `internal/api/v1/lending.go`.
