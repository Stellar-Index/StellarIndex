---
adr: 0029
title: soroban_events raw-event landing zone
status: Superseded
date: 2026-05-25
supersedes: []
superseded_by: [0034]
---

# ADR-0029: `soroban_events` raw-event landing zone

## Context

Each new per-source decoder needed a fresh multi-hour walk of the 50M-ledger Soroban era out of MinIO, only to extract "which events happened in this range". The decode itself is a pure function of the event.

## Decision

Every Soroban contract event the dispatcher routes is also written, raw, to one Postgres hypertable, `soroban_events` (migration 0041), through a `RawEventSink` hook on the dispatcher (`internal/sources/sorobanevents`). It is additive: per-source decoders keep writing their own tables. Contract id is stored as C-strkey and raw bytes; topics and body as raw XDR, with `topic_0_sym` only a convenience column; `op_args_xdr` carries the originating InvokeContract arguments. Inserts are idempotent on `(ledger, tx_hash, op_index, event_index)`. The sink blocks on a full buffer instead of dropping rows.

ADR-0034 supersedes this as the raw landing zone: the lake is ClickHouse (`stellar.contract_events`) and per-source tables are projections (ADR-0032). The Postgres sink remains wired in the indexer.

## Invariant

Event bodies are stored as raw XDR so i128/u128 amounts keep full precision (ADR-0003). The sink never drops a row silently: lost ledger ranges are logged for operator re-derivation from the lake, and the projector reads `soroban_events` only up to rows the sink has settled (`AsyncSink.Sync` barrier).

## Consequences

- A decoder backfill can be an `INSERT ... SELECT` over this table instead of a MinIO walk.
- Cost: hundreds of GB of storage and ~5-20 inserts per ledger; sustained Postgres pressure slows the dispatcher by design.

## Evidence

- `migrations/0041_create_soroban_events.up.sql`, `internal/storage/timescale/soroban_events.go`.
- `internal/dispatcher/dispatcher.go` (`RawEventSink`), `cmd/stellarindex-indexer/main.go` (sink wiring), `internal/sources/sorobanevents/dispatcher_adapter.go`.
