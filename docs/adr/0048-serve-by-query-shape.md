---
adr: 0048
title: Serve by query shape — the account-movement archive is ClickHouse-native
status: Accepted
date: 2026-07-10
supersedes: []
superseded_by: null
---

# ADR-0048: Serve by query shape — the account-movement archive is ClickHouse-native

Amends ADR-0047 D1 (storage) and sharpens ADR-0034's lake/served
split. Decided with @ash 2026-07-10 after a week of replay operations
made the cost model visible.

## Context

ADR-0034 split storage by ROLE: ClickHouse is the certified raw lake,
Postgres the served tier. In practice we drifted toward "served tier =
everything the API touches," which pulled archive-scale immutable
history toward Postgres — ADR-0047 D1 planned ~10B classic-movement
rows there. Meanwhile a week of backfill operations exposed two costs:

1. **Population.** Filling Postgres through the projector's live-tail
   machinery is Interval-bound (~720k ledgers/hour ceiling: one
   BatchLimit window per tick) — multi-day walks per source, serially.
   Lake→lake derivation of the same data is an OLAP job measured in
   hours.
2. **Serving.** The one genuinely archive-scale user story — "enter an
   address, see everything it has ever done" — is
   `WHERE address = X ORDER BY time` over 10–20B immutable rows. On a
   single-host Postgres the (address, time) index alone is hundreds of
   GB and the write amplification during population is exactly what
   ADR-0034 declared infeasible. In ClickHouse, a table sorted by
   (address, ledger) makes the same query a contiguous range read.

A survey of every current and planned user story (2026-07-10) found
that ONLY the unbounded-depth account-activity story exceeds Postgres
on our hardware. Pricing (CAGGs + point-reads + SSE), the customer
platform (OLTP), asset pages (mutable metadata), protocol pages
(10⁵–10⁷-row event tables), and current-state positions are all
PG-shaped and unaffected. External validation: the widely-cited
OpenAI Postgres-scaling write-up scales read-heavy OLTP with replica
fleets while explicitly keeping analytical scans OFF Postgres — the
same discipline, from the opposite direction.

## Decision

**D1 — Split serving by QUERY SHAPE, not by data age.**
Postgres/Timescale serves OLTP, pricing aggregates, mutable state, and
bounded protocol tables — unchanged. ClickHouse serves archive-scale
immutable history through DEDICATED sorted serving tables (never the
raw lake tables directly).

**D2 — `account_movements` (ClickHouse) replaces ADR-0047 D1's
Postgres `classic_movements` as the movement archive.**
- Feed-shaped: **two rows per movement** — one per participant — with
  a `direction` discriminator (`sent` / `received` / `self`), engine
  ReplacingMergeTree (idempotent re-derivation), ORDER BY
  `(address, ledger, tx_hash, op_index, leg_index, direction)`.
- Carries the same movement_kind/provenance taxonomy as ADR-0047 D1.
- DDL lives in `deploy/clickhouse/` (the lake DDL convention), not
  `migrations/`.
- Postgres migration 0105 (`classic_movements`) stays applied but
  UNPOPULATED — documented as superseded-by-0048; dropped in a later
  cleanup migration once the CH path is proven.
- ADR-0047's decode layer (`internal/sources/classicmovements`), the
  CH op reader, the recognition guards, and the P23 clamp are reused
  unchanged; only the WRITE target moves. The derivation
  (`classic-movements-backfill`) becomes lake→decode→CH — no Postgres
  in the loop.

**D3 — Bulk derivation path for projected sources
(`projected-rebuild`).**
Projected-source catch-up beyond a trivial lag stops using
`projector-replay` (live-tail machinery: tick cadence + 60s
deadlines). A new `stellarindex-ops projected-rebuild` runs parallel
ledger-window workers that stream lake events through the SAME
registry-built decoders and sink (identical rows, ON CONFLICT
idempotent), checkpointed and resumable, with no tick ceiling.
`projector-replay` remains for small rewinds (< ~1M ledgers).
One-writer-per-domain is preserved: the bulk path and the live
projector write identical idempotent rows and never run concurrently
for the same source (the tool rewinds/parks the source cursor while it
owns the range).

**D4 — Serving protection.** All CH-serving queries run under a
dedicated ClickHouse settings profile (bounded threads/memory,
priority above merges and backfill inserts), codified in ansible.
Backfills keep running under `run-heavy-job.sh` + CH quotas. A
serving query must never queue behind a derivation.

**D5 — The unified account feed** (`/v1/accounts/{g}/movements`)
merges the CH archive with the Postgres `sep41_transfers` recent tail
at read time (per ADR-0047 D1's read-surface plan). Optionally, a
later phase materializes the CAP-67 era into `account_movements` too
(provenance `cap67_event` is already reserved) — deferred until the
pre-P23 archive is proven.

## Consequences

- The movement backfills become CH→CH-shaped jobs (hours, rerunnable)
  instead of a multi-week Postgres write campaign; decoder-bug
  re-derives in this domain become cheap, which lowers the cost of
  the correction loop the whole architecture is built around.
- The held r1 replays (blend_backstop, blend_emitter,
  aquarius-rewards history) run via D3 at roughly 10–20× the
  projector-replay rate.
- Postgres stays lean: its growth is bounded to protocol tables and
  the pricing working set, per ADR-0034's original intent.
- Two serving stores means the account feed's read path has a merge
  seam (CH archive + PG tail) — accepted; it is provenance-honest and
  each side is independently verifiable (ADR-0033 applies per store).
- CH becomes a serving dependency for one public surface; D4's
  isolation profile is a hard prerequisite for shipping D5, given the
  2026-07 CH resource incidents.
