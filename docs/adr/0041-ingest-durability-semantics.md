---
adr: 0041
title: Ingest durability semantics — cursor advance, lake sink defaults, drop visibility
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0041: Ingest durability semantics

## Context

The ledgerstream cursor advances when a ledger's events are enqueued, not when they are durably written, so a hard crash loses whatever was buffered.
The lake sink drops whole ledger extracts under buffer pressure by design, healed by `ch-live-catchup`. The "100% coverage" claim rests on the lake capturing everything.

## Decision

1. **Cursor semantics.** Keep advance-on-enqueue. Advancing only past the minimum durable watermark of all sinks would couple live throughput to the slowest sink. The loss window is one crash times the buffer depth, every lost row is recoverable from the lake, and the ADR-0033 per-ledger verdict detects any residue. The cursor is a resume hint, not a durability claim. The completeness verdict is the durability claim.
2. **Lake defaults on.** `clickhouse_live_sink` and `clickhouse_projector_source` default to `true`, matching the only production topology. A deployment that cannot run ClickHouse opts out explicitly and loses the certified-lake substrate, the CH completeness path and lake-derived supply.
3. **Drops are alerted.** `stellarindex_ingestion_ch_live_sink_drops` is a ticket (any drop increase over 10m, `for: 10m`). `stellarindex_ingestion_ch_live_sink_drops_sustained` is a page (drops persisting 1h, the heal path losing the race).
4. **Lake reads carry a watermark.** Lake-backed responses stamp `as_of_ledger` from the lake's contiguous watermark (`ExplorerReader.LakeWatermark`, clamped to the contiguous tip), and `flags.stale` fires when it lags.

## Invariant

- A change that weakens the verdict's ability to see a post-crash hole is a regression against this ADR, for example disabling the completeness timer or moving to aggregate-only reconciles.
- The two lake config defaults stay `true` (`internal/config/config.go`), and config validation rejects `clickhouse_projector_source = true` with `clickhouse_live_sink = false` (`internal/config/validate.go`).
- Sustained live-sink drops page. The two alerts are in `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml`, and `deploy/monitoring/rule-tests/ingestion-core_test.yml` covers them.
- Lake-backed supply and state reads surface the lake watermark as `as_of_ledger` and set `flags.stale` against it.

## Consequences

- Fresh deployments capture the lake by default. A crash's loss window remains, but it is bounded, detected, healable from the lake, and visible within minutes through the drop alert.
- This covers lake-backed on-chain ingest only. The CEX/FX sinks do not flow through the lake and need their own backpressure treatment.

## Evidence

`cmd/stellarindex-indexer/main.go` (`processAndPersistCursor`), `internal/storage/clickhouse/live_sink.go`, `internal/storage/clickhouse/protocol_reader.go`, `deploy/monitoring/rules/ingestion.yml`.
