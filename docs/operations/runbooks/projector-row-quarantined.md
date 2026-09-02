---
title: Runbook — projector row quarantined
last_verified: 2026-07-24
status: draft
severity: P3
---

# Runbook — `stellarindex_projector_row_quarantined`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_projector_row_quarantined` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/projector.yml` + `configs/prometheus/rules.r1/projector.yml` — `increase(stellarindex_projector_events_decoded_total{outcome="sink_quarantined"}[15m]) > 0` for 15m |
| Typical MTTR | minutes to diagnose; the re-drive itself is usually fast once the underlying defect is fixed |
| Impact | ONE event is durably skipped from the served tier for the affected source. Not data loss (the raw event still lives in the ClickHouse lake), but it IS a real, silent gap in the projected/served data until an operator re-drives it. |

## Why this exists

COR-11/COR-01 (audit-2026-07-23): the ADR-0032 projector's sink can hold
a row that fails deterministically forever (a poison row) — before this
fix, that single row would wedge the projector's cursor for its source
permanently, since the cursor can only advance past a ledger once every
event in it has fully committed. `quarantineCandidate` (see
`internal/projector/projector.go`) now gives up on **at most one** held
row per cycle once its per-row retry budget is exhausted, bumps
`stellarindex_projector_events_decoded_total{source,outcome="sink_quarantined"}`,
and lets the cursor advance past it. This alert is the only signal that
a quarantine happened — without it, an operator has no way to know a
row silently dropped out of the served tier.

## Symptoms

- `stellarindex_projector_events_decoded_total{source="X",outcome="sink_quarantined"}` incremented.
- Indexer journal: `projector: QUARANTINED un-processable row after
  exhausting the retry budget — cursor advances past it; re-drive with
  \`stellarindex-ops projector-replay\` once fixed`, with the exact
  `ledger` / `tx` / `op_index` / `event_index` of the skipped row.
- The projector's lag for that source (`stellarindex_projector_lag_ledgers`)
  should otherwise be normal — quarantine is what UNSTICKS the cursor, so
  this alert can fire even while lag looks healthy.

## Quick diagnosis (≤ 5 min)

```sh
# Find the exact quarantined row(s) for the source.
journalctl -u stellarindex-indexer --since -1h | grep -i "QUARANTINED"

# Confirm the metric and how many rows have been quarantined recently.
curl -s http://indexer:9464/metrics | grep 'stellarindex_projector_events_decoded_total{.*sink_quarantined'
```

Each quarantine log line carries the row's `ledger`, `tx`, `op_index`,
`event_index`, `consecutive_cycles`, and the underlying `err` — that
error is almost always the actual root cause (a decoder bug, a
downstream PK/constraint violation, or a malformed on-chain payload).

## Mitigation (≤ 15 min to start)

- [ ] Read the quarantine log line's `err` field — this tells you WHY the
      row was un-processable (decoder panic/bug, PK collision, bad
      contract data, etc.). Fix the underlying defect first; re-driving
      without fixing it just re-quarantines the same row.
- [ ] Once fixed, re-drive the specific row/range with
      `stellarindex-ops projector-replay -source <X> -from <ledger> -write`
      (the raw event is still in the lake — nothing to re-derive from
      scratch).
- [ ] Verification: the re-drive succeeds and no new
      `sink_quarantined` increments occur for that source; the ADR-0033
      completeness verdict for the source stays/returns to `complete=true`
      (see `completeness-incomplete.md`) after the next
      `compute-completeness` run.

## Root cause analysis

Quarantine is always the SECOND-order signal — the real defect is
whatever made the row deterministically un-processable
(`internal/projector/sinkfault.go` classifies the failure). Common
causes: a decoder regression that panics on a specific payload shape, a
downstream table's PK/unique constraint rejecting the row for a reason
that isn't transient (e.g. a duplicate the sink didn't expect), or
malformed/unexpected on-chain data for a contract version the decoder
doesn't handle. Check the `err` in the quarantine log line first — it
names the failure directly.

## Known false-positive patterns

None known yet — a quarantine is by construction a rare, deliberate
event (at most one per source per cycle), so any occurrence should be
investigated rather than dismissed. If a specific source starts
quarantining rows repeatedly, that is itself the signal of a systemic
decoder or schema regression, not noise.

## Related

- `projector-lag.md` — the projector's primary lag/cycle-error alerts;
  quarantine is a distinct, rarer failure mode from the same subsystem.
- `completeness-incomplete.md` — a sustained quarantine (or several) on
  one source will eventually show up as `complete=false` on the ADR-0033
  verdict.
- `internal/projector/projector.go` (`quarantineCandidate`) and
  `internal/projector/sinkfault.go` — the implementation.

## Changelog

- 2026-07-24 — initial draft, added alongside the alert (audit-2026-07-23
  missing-alert-negativespace remediation).
