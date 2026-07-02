---
title: Architecture overview — the 10-minute orientation
last_verified: 2026-07-02
status: current
---

# Architecture overview

This is the orientation page CLAUDE.md and engineering-standards.md
point at (it did not exist until 2026-07-02 — both cited it anyway).
It deliberately says nothing the deeper docs don't; its job is to
route you.

## The system in one paragraph

**Stellar Index** captures every ledger of the Stellar network from a
self-hosted Galexie archive into a certified **ClickHouse raw lake**
(ADR-0034), decodes protocol activity through a **dispatcher** of
per-source decoders into a **TimescaleDB served tier** (with a
**projector** as the single writer for Soroban-derived tables,
ADR-0031/0032), verifies the whole chain end-to-end with a
three-claim **completeness verdict** (ADR-0033), aggregates trades
into **VWAP/TWAP/OHLC** with exact rational arithmetic (ADR-0003),
and serves it all through a public **REST + SSE API** and a static
**explorer** at stellarindex.io.

## The five load-bearing flows

| Flow | Path | Deep doc |
|---|---|---|
| On-chain ingest | Galexie MinIO → `ledgerstream` → `dispatcher` → decoders → sink/projector → Timescale (+ CH dual-sink) | [ingest-pipeline.md](ingest-pipeline.md) |
| Off-chain ingest | CEX/FX connectors (`sources/external`) → same event channel | CLAUDE.md "Add a new CEX connector" |
| Aggregation | trades → outlier filter → class gating → VWAP → freeze/confidence → Redis + CAGGs | [aggregation-plan.md](aggregation-plan.md) |
| Verification | lake substrate + recognition + per-ledger projection reconcile → `completeness_snapshots` | ADR-0033, ADR-0041 |
| Serving | Timescale CAGGs + Redis + CH explorer reads → `internal/api/v1` → REST/SSE | ADR-0015/0018, [platform-spec.md](platform-spec.md) |

## Where truth lives

- **Raw truth:** the ClickHouse lake (hash-chained to genesis;
  re-derivable from the Galexie archives — ADR-0043).
- **Served truth:** TimescaleDB — verified faithful to the lake
  per-ledger by the daily verdict.
- **Value truth:** `verify-served-values` reconciles flagship served
  numbers against independent sources (SDF, Stellar Expert).
- **Contract truth:** `openapi/stellar-index.v1.yaml` — handlers,
  SDK, and explorer types are all machine-reconciled against it.

## Reading order for a new engineer/agent

1. `CLAUDE.md` (the map — includes the invariants and the traps)
2. This page
3. [ingest-pipeline.md](ingest-pipeline.md) — the binding ingest rules
4. ADR-0033 + ADR-0034 — the trust story
5. `docs/engineering-standards.md` — the policy you're bound by
6. The `doc.go` of whatever package you're touching
