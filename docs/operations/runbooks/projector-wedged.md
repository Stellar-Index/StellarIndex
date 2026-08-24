---
title: Runbook — projector-wedged
last_verified: 2026-08-25
status: ratified
severity: P3
---

# Runbook — `stellarindex_projector_wedged`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_projector_wedged` |
| Severity | P3 (ticket — no customer-facing outage, but the source will NOT self-recover) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/projector.yml` (gauge set by `internal/projector/projector.go::cycleOneSource`) |
| Typical MTTR | 15 min (raise the budget) – hours (decompress a range) |
| Impact | The named per-source projection (trades, `phoenix_*`, `blend_*`, …) stops advancing; the served tier for that source freezes behind the raw lake. Every OTHER source keeps flowing. No data loss — the raw events are durable in the ClickHouse lake / `soroban_events`. |

## Symptoms

- `stellarindex_projector_wedged{source="…"}` reads `1` for 5+ minutes.
- `stellarindex_projector_lag_ledgers{source="…"}` has stopped falling
  (flat, not climbing back down).
- `rate(stellarindex_projector_runs_total{source="…",outcome="error"}[15m])`
  is non-zero and steady — the source cycles, errors, and never advances.

## What "wedged" means

The projector adapts its per-source read window: on a per-cycle deadline
(`PerSourceTimeout`, 60s) it halves the window down to the `MinBatchLimit`
floor of 25 ledgers, so a dense stretch converges instead of retrying a
huge range forever. A **wedge** is the terminal case that adaptation
cannot escape: a *floor-sized* (25-ledger) range that **still** takes
longer than `PerSourceTimeout` to decode + sink — a dense window over a
**compressed** ClickHouse chunk. There is nothing left to halve, so the
cursor holds and the identical 25-ledger range is retried every cycle
forever. `cycleOneSource` flips the gauge to `1` once the source has sat
at the floor and failed to advance for `WedgeCycles` (5) consecutive
cycles; any advancing cycle clears it.

This is the same failure class as the 2026-07-10 aquarius-rewards stall
and the 2026-08-01 aquarius-reserves stall (wedged 3.5h at ledger
63,488,687) — previously visible only as "lag stopped falling".

## Quick diagnosis (≤ 5 min)

```sh
# Which source(s) are wedged, and for how long?
#   promql: max by (source) (stellarindex_projector_wedged) > 0

# Confirm the cursor is genuinely stuck (lag flat, cycle errors steady):
#   promql: stellarindex_projector_lag_ledgers{source="<src>"}
#   promql: rate(stellarindex_projector_runs_total{source="<src>",outcome="error"}[15m])

# The projector logs the held range each cycle. The `from`/`to` span is
# the wedged 25-ledger range; note whether the chunk it lands in is
# compressed.
ssh r1 'journalctl -u stellarindex-indexer --since "30 min ago" --no-pager \
  | grep -i "shrinking window\|holding cursor" | grep "<src>" | tail -20'
```

## Mitigation

Remediation is **manual and operator-owned** — the projector deliberately
does not auto-raise the budget or auto-decompress (either could starve the
host or thrash merges under load). Pick the smallest safe lever:

- [ ] **Decompress the offending range** (preferred when the wedge is over a
  single cold/compressed chunk). Recompressing the partition that holds the
  wedged range to a faster/idle-decompressible codec — or materialising the
  range into an uncompressed staging part — lets the 25-ledger window finish
  inside the budget. Confirm the wedge clears afterward.
- [ ] **Raise the per-cycle budget** (`PerSourceTimeout`) if the range is
  legitimately heavy and decompression is not practical right now. This is a
  code/config change + redeploy of the indexer; size it so the floor window
  finishes with headroom, and watch that other sources' cycle latency stays
  healthy.
- [ ] **Verification**: `stellarindex_projector_wedged{source="…"}` returns to
  `0` and `stellarindex_projector_lag_ledgers{source="…"}` resumes falling.
  A clean advance clears the gauge automatically on the next cycle.

## Known false-positive patterns

- **A single very slow cycle** does NOT wedge — the flag needs `WedgeCycles`
  (5) consecutive floor-stalls, so a one-off slow decompress clears itself.
- **Still-shrinking source**: a source blowing the deadline while the window is
  *above* the floor is adapting normally and is not flagged (the window is
  still halving toward a size that fits).

## Related

- `stellarindex_projector_lag_high` — the softer, self-recovering lag signal;
  a wedge is the case where lag stops falling for good. See
  [projector-lag](projector-lag.md).
- [projector-replay](projector-replay.md) — re-drive machinery once the
  underlying density/compression cause is fixed.
- ADR-0032 — the projector-as-single-writer design this cursor anchors.

## Changelog

- 2026-08-25 — initial draft alongside the task #33 / W8 recon 9b wedge gauge.
