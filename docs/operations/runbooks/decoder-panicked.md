---
title: Runbook — stellarindex_decoder_panicked
last_verified: 2026-09-02
status: active
severity: P1
---

# Runbook — `stellarindex_decoder_panicked`

## At a glance

| | |
|---|---|
| **Alert** | `stellarindex_decoder_panicked` — `stellarindex_decoder_panics_total > 0` |
| **Severity** | page |
| **What it means** | A source decoder's `Matches` or `Decode` PANICKED on a ledger input. The dispatcher recovered it, skipped that one input, and ingest continued — the process is up and the cursor is advancing. A panic in `Decode` costs one event for one source; a panic in `Matches` aborts that whole dispatch chain, so other decoders that would have matched the SAME input are skipped too — one input, not one source. |
| **First action** | Read the `source` label, pull the `decoder panicked` journal line (it carries `ledger` / `tx_hash` / `op_index` / `stack`), and treat it as a decoder bug. |
| **Why page** | The decoder will keep silently dropping **every** event of that shape until a fixed binary ships. Nothing else fires: the ledger completes, the cursor advances, and only the ADR-0033 coverage verdict eventually notices. |
| **Data loss** | None permanent. The raw event is already in the ClickHouse lake — re-derive after the fix. |

## Why this exists

Before #371 F1 there was no `recover()` anywhere in `internal/dispatcher`.
A panic in a decoder unwound out of `ProcessLedger` and was caught only at
LEDGER granularity by `internal/pipeline/processor.go:41`, which returned
`dispatcher panic for ledger N`. That discarded the outputs of **every**
source for that ledger, refused the cursor advance, and the indexer's
`realMain` returned the error → process exit → `Restart=on-failure` →
re-read the same ledger from the same cursor → same panic. After
`StartLimitBurst` restarts systemd parked `stellarindex-indexer` in
`failed` and all ingest stopped until a human intervened. One decoder's
bug was a total, self-sustaining ingest outage.

The dispatch seams already had a policy for "this decoder cannot handle
this input": count it, skip that one input, let the ledger finish. A panic
now gets the same handling — so the blast radius is one input for one
source instead of every source forever — and this alert is what makes the
skip loud instead of silent.

## Symptoms

- `stellarindex_decoder_panicked{source="<name>"}` firing (fires on the
  first panic, and stays firing until the process restarts).
- Journal line `decoder panicked — input SKIPPED, ingest continues` on
  `stellarindex-indexer`, with `decoder`, `ledger`, `tx_hash`,
  `op_index`, `panic` and a `stack`.
- `stellarindex_source_decode_errors_total{source=…}` moved by the same
  amount — a recovered panic is counted as a decode error too, so the
  existing decode-error signal and `decoder_stats` stay consistent.
- Downstream, hours later: `/v1/coverage` reports `complete=false` for
  the affected window once `compute-completeness` re-derives it (below).

## Quick diagnosis (≤ 5 min)

```sh
# Which decoder, which ledger, what was the fault?
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "-2h" --no-pager | grep -A25 "decoder panicked"'

# How many, and is it still climbing? (One-offs and storms need
# different responses; a storm means an event SHAPE, not one bad event.)
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_decoder_panics_total" | python3 -m json.tool | grep -E "source|value"'

# Sanity: ingest is genuinely still moving (it should be — the whole
# point of the guard). If it is NOT, this is a different incident.
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_cursor_last_ledger" | python3 -m json.tool | grep -E "source|value"'
```

## How the skipped event stays visible (ADR-0033)

This is why "skip and continue" is not the same as "silently lose data".
The skip is recorded in three independent places, none of which depends
on the indexer process surviving:

1. **The raw event is in the ClickHouse lake already.** `dispatchOne`
   pushes to the raw-event sink BEFORE the decoder pass, and the lake
   extractor (`internal/storage/clickhouse` `ExtractLedger`, wired at
   `cmd/stellarindex-indexer/main.go`) is decoder-independent. So the
   substrate keeps its genesis-to-tip claim and `lake_complete` is
   unaffected — nothing is lost, it is only un-projected.
2. **The coverage VERDICT turns red.** `compute-completeness` re-derives
   each source's outputs from the lake through the SAME decoder, under
   `safeDecode` (`internal/completeness/reconcile.go`, the
   `decoder panicked on event shape` arm). A panicking decoder marks the
   ledger `blind.Undecodable` → `BlindSpots` → `projection_ok=false` →
   `/v1/coverage` reports `complete=false` for that window. The verdict
   tells the truth about the gap rather than papering over it.
3. **The decode-error delta is persisted.** `statsflush` writes the
   per-source decode-error count to `decoder_stats` every 5 minutes, and
   `ledger_ingest_log` (migration 0051) carries the decoder-independent
   census for the same ledger.

For classic op decoders (sdex) the `classic_trade_effect_count` census
vs `trades` reconcile shows the delta; for entry-change observers the
per-class gap detector does.

## Mitigation (≤ 15 min)

There is no operational mitigation — a deterministic decoder panic needs
a code fix. What you can do now is bound it and prove it is bounded.

- [ ] **Do NOT restart the indexer to "clear" the alert.** The counter is
      process-lifetime; a restart resets it to absent and hides the bug.
      The guard means the unit is healthy — leave it running.
- [ ] Confirm ingest is unaffected: the cursor is advancing and
      `stellarindex_ingestion_all_sources_stopped` is not firing.
- [ ] Decide the blast radius from the panic RATE. A single increment is
      one poison event. A counter climbing every ledger means the decoder
      panics on a whole event SHAPE (a contract upgraded its event
      schema — see docs/architecture/contract-schema-evolution.md), so
      that source is effectively dark from this ledger on.
- [ ] If the source must not run blind in the meantime, remove it from
      `ingestion.enabled_sources` in `/etc/stellarindex.toml` (via the
      ansible role — configuration is ansible-managed) and restart. This
      is a deliberate, recorded gap rather than a silent one.
- [ ] File the bug with the stack + `(ledger, tx_hash, op_index)` from
      the journal line.

## Recovery after the decoder is fixed

Re-derive from the lake — never a MinIO re-walk (invariant 8).

```sh
# Projected sources (soroswap, blend, phoenix, comet, defindex, sep41_*,
# reflector/redstone oracle_updates, …):
stellarindex-ops projector-replay -source <name> -from <ledger>

# Non-projected sources (sdex, band, supply observers):
stellarindex-ops ch-rebuild ...   # see docs/operations/backfill-procedure.md

# Then re-run the verdict for the window and confirm it goes green:
stellarindex-ops compute-completeness ...
```

## Root cause analysis

A decoder panic is always a code defect: an unchecked slice index, a
`MustI128()` on a map-shaped `transfer` body, a nil deref on an optional
field. The two recurring generators in this codebase are:

- **Contract schema evolution.** Soroswap / Phoenix / Aquarius /
  Reflector `update_contract` in place; a backfill replays every prior
  WASM version. Decode by Map-field-name, not position.
- **Adversary-shaped input.** Event bodies are attacker-influenced. A
  decoder is parsing untrusted bytes and must never assume arity.

Attach the stack, the `(ledger, tx_hash, op_index)`, and the raw event
pulled from the lake for that coordinate.

## Known false-positive patterns

None. A recovered decoder panic is never benign — it means events of that
shape are being dropped from the served tier right now.

Note the alert does NOT self-clear: `> 0` on a process-lifetime counter
keeps firing until the binary restarts. That is deliberate (the decoder
is still broken), and it is why the expression is not `increase(...)`: a
one-off panic creates a series that is born at 1 and never moves, and
`increase()` over a series that appears inside its own lookback window
evaluates to 0 — the alert would never fire for the single-poison-event
case it exists to catch.

## Related

- #371 F1 (REL dependency-failure matrix) — the finding this closes.
- `internal/dispatcher/panic_guard.go` — the guard; `dispatcher.go`'s
  four dispatch seams are where it is installed.
- `internal/completeness/reconcile.go` — `safeDecode`, the ADR-0033 arm
  that turns the same panic into a visible blind spot.
- [worker-panicked](worker-panicked.md) — the sibling alert for a
  panicking background WORKER (that one stops the worker; this one skips
  one input).
- [decode-errors](decode-errors.md) — the per-source decode-error-rate
  alert; every panic also increments that counter.
- [frozen-indexer-cursor](frozen-indexer-cursor.md) — use if the cursor
  is NOT advancing (a different incident).
- docs/architecture/contract-schema-evolution.md; ADR-0033; ADR-0034.

## Changelog

- 2026-09-02 — created with the metric + guard (#371 F1).
