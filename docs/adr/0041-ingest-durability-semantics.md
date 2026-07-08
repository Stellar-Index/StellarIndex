---
adr: 0041
title: Ingest durability semantics — cursor advance, lake sink defaults, drop visibility
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0041 — Ingest durability semantics

- **Closes:** CS-028 (cursor advances on enqueue), the "substrate off
  by default" trap, and the LiveSink silent-drop blind spot (audit
  2026-06-30 / cold-read 2026-07-02).

## Context

Three coupled facts about the live ingest path:

1. **The ledgerstream cursor advances on ENQUEUE, not durable write**
   (`processAndPersistCursor`): events sit in the PersistEvents
   channel, the `soroban_events` AsyncSink, and the CH LiveSink when
   the cursor moves past their ledger. A hard crash loses whatever
   was buffered — silently, for any source the census doesn't cover.
2. **The ClickHouse dual-sink and the projector's CH feed-switch were
   `false` by default**, while the "100% coverage" claim rests on the
   CH lake capturing everything. r1 — the only deployment — runs both
   `true`; the defaults described a deployment shape we don't run and
   would hand R2/R3 bring-up a certified-lake claim with the lake
   sink off.
3. **`clickhouse.LiveSink` drops whole ledger extracts on buffer
   pressure by design** (non-blocking; `DroppedCount++`), healed by
   the `ch-live-catchup` timer — but the
   `stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}`
   counter had **no alert in either rule tree**: sustained drops
   (wedged CH, undersized buffer) would surface only as a later
   completeness-verdict failure, hours after the fact.

## Decision

### 1. Cursor semantics: keep enqueue-advance; the lake + reconcile IS the durability story

We considered advancing the cursor only past the minimum durable
watermark of all sinks. Rejected for now:

- It couples live throughput to the SLOWEST sink (the 20× throughput
  win of the batched channel drain came precisely from decoupling).
- The loss window is one crash × ≤ buffer depth, and every lost row
  is recoverable **mechanically**: raw events from the CH lake
  (`ch-live-catchup` heals the lake's own tail; `projector-replay` /
  re-derive heals the served tier), and the ADR-0033 verdict — since
  CS-084, strict per-ledger — DETECTS any residue.
- The invariant we commit to instead: **the cursor is a resume hint,
  not a durability claim.** The completeness verdict is the
  durability claim. Anything that weakens the verdict's ability to
  see a post-crash hole (e.g. disabling the daily timer, aggregate
  reconciles) is a regression against THIS ADR, not just ADR-0033.

Preconditions this rests on (now enforced/monitored):
- the CH sink is on (Decision 2),
- the verdict runs on a timer and is watchdogged (`data-freshness`),
- drops are alerted (Decision 3).

### 2. `clickhouse_live_sink` + `clickhouse_projector_source` default to `true`

The defaults now match the only production topology and the coverage
claim's substrate. Deployments that genuinely cannot run ClickHouse
(dev laptops, minimal self-hosters) opt OUT explicitly — the config
docs say what they give up: the certified-lake substrate, the CH
completeness path, and lake-derived supply. r1 is unaffected (both
already explicitly `true`).

### 3. Sustained LiveSink drops page

New alert `stellarindex_ingestion_ch_live_sink_drops` (both rule
trees): any drop increase over 10m, `for: 10m`, severity ticket —
drops are self-healing via `ch-live-catchup`, so this is "the heal
path is being exercised abnormally", not a page. A companion
`severity: page` threshold fires when drops persist for 1h (the heal
path is losing the race — lake tail integrity at risk).

### 4. CH current-state reads get a staleness signal (spec)

`ledger_entries_current` and `supply_flows` reads serve whatever the
lake holds, with no indication when `ch-live-catchup` is behind. The
committed follow-up: readers surface the lake's contiguous watermark
(`ContiguousWatermark`, already computed for the projector clamp)
as an `as_of_ledger` on supply/state responses, and the API's
`flags.stale` fires when `tip - as_of_ledger` exceeds the freshness
window. Implementation rides with the reconciliation harness work
(board #14) — the same watermark plumbing serves both.

## Consequences

- Fresh deployments capture the lake by default; the "certified raw
  history" claim stops being config-conditional.
- A crash's loss window remains, but is bounded, detected (strict
  per-ledger verdict), healable (lake re-derive), and now *visible
  within minutes* (drop alert) instead of at the next verdict run.
- Operators explicitly opting out of ClickHouse own the consequence
  in their config file, in writing.
- **Acceptance caveat (2026-07-08):** this ADR's durability story
  covers lake-backed (on-chain) ingest only — the non-lake CEX/FX
  sinks do not flow through `soroban_events`/ClickHouse and need
  their own backpressure/durability treatment (tracked separately;
  see the trade-insert-backpressure alert lineage).
