---
title: Runbook — freeze-recovery-stalled
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_anomaly_freeze_recovery_stalled`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_anomaly_freeze_recovery_stalled` (P3, `severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/anomaly.yml` (r1 overlay, the file r1 actually loads; multi-host twin: `deploy/monitoring/rules/anomaly.yml` — the alert sits at ~line 133 in both). Expr: `sum(rate(stellarindex_anomaly_freeze_engaged_total[1h])) > sum(rate(stellarindex_anomaly_freeze_recovered_total[1h]))` AND `on() (sum(rate(stellarindex_anomaly_freeze_recovery_sweeps_total{outcome!="ok"}[15m])) > 0)`, sustained `for: 2h`. |
| Typical MTTR | 5–15 min once the cause is found |
| Impact | Resolved freezes still appear "firing" in `freeze_events`; the explorer `/anomalies` timeline misrepresents resolved incidents as ongoing. The API itself is unaffected — `flags.frozen` is driven by the Redis marker, not the durable mirror. |

## What "stalled" means

The aggregator runs two sides of the freeze pipeline (post-migration-0119
lifecycle — `internal/aggregate/freeze/lifecycle.go`):

1. **Writer side** (`internal/aggregate/freeze.Writer`) — on every
   `TransitionFired` the orchestrator writes a Redis marker and INSERTs a
   row into `freeze_events` with `recovered_at = NULL`. A freeze is a HOLD
   with an extension ladder, not a per-bucket decision: the initial hold is
   **30 minutes** (10 minutes when no corroborating lens produced a reading
   at fire time), and each unearned expiry extends by **30 minutes**, up to
   4 extensions before escalating to operator review. The marker's TTL is
   **the remaining hold + `cachekeys.FreezeTTL`**. `FreezeTTL` (5 min) is
   the SILENCE GRACE — how long a freeze survives aggregator silence — NOT
   the freeze duration. Since 0119 the ladder is also mirrored to the
   `freeze_events` row itself (`hold_until`, `extensions_used`,
   `escalated`, `corroborated` columns), so it survives losing Redis.
2. **Recovery side** (`internal/aggregate/freeze.Recovery`,
   `internal/aggregate/freeze/recovery.go`) — every 60 s (plus one
   immediate sweep at startup), lists every open `freeze_events` row and
   checks whether the Redis marker for that pair still exists. A
   marker-gone row is closed (`recovered_at = now()`) **only if the
   durable ladder no longer holds** (`ladderStillHolds`, using the same
   `LadderStillLive` predicate the writer uses): an open row whose
   `hold_until` + grace is still in the future is left OPEN for the
   orchestrator to rehydrate, logged as `held_by_ladder`. After a Redis
   flush, open rows therefore legitimately outnumber Redis markers —
   that is the 0119 protection working, not a stall.

When recovery genuinely stalls, `stellarindex_anomaly_freeze_engaged_total`
keeps incrementing while `stellarindex_anomaly_freeze_recovered_total`
flatlines, sweeps report non-`ok` outcomes, and the backlog of open rows in
`freeze_events` grows.

## Symptoms

- `sum(rate(stellarindex_anomaly_freeze_engaged_total[1h])) >
  sum(rate(stellarindex_anomaly_freeze_recovered_total[1h]))` for ≥ 2 h
- `sum(rate(stellarindex_anomaly_freeze_recovery_sweeps_total{outcome!="ok"}[15m])) > 0`
  — the alert's own clause. (An earlier draft suggested
  `count by () (max_over_time(...{outcome="error"}[15m]))`, which is a
  cumulative-counter read: non-zero forever after a single historical
  error. Use the `rate()` form.)
- `SELECT count(*) FROM freeze_events WHERE recovered_at IS NULL`
  on r1 postgres returns a number well above the count of freezes that
  are plausibly live (steady-state: markers in Redis + any rows whose
  durable hold is still running)
- Aggregator logs show `component=freeze-recovery` WARN lines (`list open
  freezes`, `MarkRecovered failed`, `Redis Get failed during recovery
  sweep`)

## Quick diagnosis (≤ 5 min)

```sh
# 1) Is the recovery goroutine running? The sweep counter increments
# every 60 s on EVERY outcome (including a no-op sweep with zero open
# rows). Sample twice, 2 min apart — increasing on ANY outcome label
# means the goroutine is alive.
ssh root@136.243.90.96 "curl -s http://localhost:9465/metrics | grep stellarindex_anomaly_freeze_recovery_sweeps_total"
# (Do NOT grep the logs for Debug "recovery sweep complete" lines: that
# line is Debug-level — invisible at the default info log level — and a
# sweep with zero open rows logs nothing at all. Absence of the line
# does not mean the goroutine is dead; do not restart a healthy
# aggregator on that evidence.)

# 2) Is it failing on the lister side (postgres) or the cache side
# (Redis)? Read the outcome labels from the same output:
# outcome="error"   → the ListOpen postgres query failed (whole sweep aborted)
# outcome="partial" → per-row failures: Redis GETs erroring, or the
#                     MarkRecovered postgres UPDATE failing for some rows

# 3) Confirm the open-row backlog directly, with the durable-ladder columns:
ssh root@136.243.90.96 "runuser -u postgres -- psql -d stellarindex -c \
  \"SELECT count(*), min(frozen_at),
          count(*) FILTER (WHERE hold_until > now()) AS holds_still_live,
          count(*) FILTER (WHERE escalated) AS escalated
     FROM freeze_events WHERE recovered_at IS NULL;\""
```

Decision tree:

| `outcome="error"` rising | `outcome="partial"` rising | Probable cause | Action |
|---|---|---|---|
| Yes | No | Postgres lister query failing OR Redis transport broken | Check postgres logs + Redis health |
| No | Yes | Per-row `MarkRecovered` UPDATE or Redis GET failing | Check postgres logs for `freeze_events` UPDATE errors; check WARN lines in the aggregator log |
| No | No, but backlog still growing | Either (a) the Redis markers are being refreshed because the anomalies genuinely persist — verify with `ssh root@136.243.90.96 "redis-cli --scan --pattern 'freeze:*'"` — or (b) markers are GONE but the durable holds are still live (`held_by_ladder`; the post-Redis-flush case). For (b) look for the INFO line "freeze marker missing but the durable hold is still live" — the worker is deliberately leaving those rows open for the orchestrator to rehydrate. Neither is a stall. | (a) → underlying-anomaly runbooks; (b) → nothing: the rows close via the normal lifecycle once the freeze actually ends |
| No | No, backlog flat near 0 | Alert is a false positive — the recovery worker is healthy | Tune the alert threshold |

## Mitigation (≤ 15 min)

- [ ] **Step 1 — Confirm whether the underlying freezes are real.**
  If the open rows match live Redis markers (or live durable holds),
  the worker is doing the right thing and the anomalies legitimately
  persist. Switch to the [anomaly-freeze-engaged](anomaly-freeze-engaged.md) +
  [anomaly-freeze-sustained](anomaly-freeze-sustained.md) runbooks
  and investigate the underlying market events.

- [ ] **Step 2 — If postgres-side error:** check
  `journalctl -u postgresql@15-main --since "30 min ago" --no-pager`.
  The most common causes are connection-pool exhaustion (the recovery
  worker uses the shared `*sql.DB` from the aggregator's store) or a
  long-running ANALYZE/VACUUM blocking the UPDATE. Restart the
  aggregator if the pool is wedged.

- [ ] **Step 3 — If Redis-side error:** check
  `redis-cli ping` and the `redis-server` systemd unit
  (`systemctl status redis-server`). If Redis is up but the recovery
  sweep still fails its GETs, suspect ACL changes (the recovery worker
  uses the same client as the freeze writer — if one works the other
  should too).

- [ ] **Step 4 — Manual close-out** if specific rows need to close
  immediately and the recovery worker remains broken. **Prefer the
  typed operator path** — it clears the marker AND stamps the row,
  and requires a recorded reason:

  ```sh
  stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
  stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
    -asset <asset_id> -quote <quote_id> -reason "recovery worker down; verified marker gone by hand"
  ```

  A bulk SQL sweep is a last resort and MUST be gated on the durable
  hold — **closing a ladder-held row is destructive**: `recovered_at IS
  NULL` is the exact predicate the 0119 rehydrate reads, so blanket-
  closing open rows deletes the ladder's Redis-flush protection (an
  escalated freeze silently loses its "stays active until manual
  unfreeze" state) and records a normal recovery on `/v1/anomalies`
  for a freeze that never recovered:

  ```sql
  -- Close only rows whose durable hold has demonstrably lapsed
  -- (10 min is comfortably past the 5-min marker/ladder grace).
  -- recovered_at_ledger stays NULL: the ledger is unknown at manual
  -- close time, and 0 would violate the CHECK constraint
  -- (migrations/0018:54 — recovered_at_ledger >= frozen_at_ledger).
  UPDATE freeze_events
     SET recovered_at = now(),
         recovered_at_ledger = NULL
   WHERE recovered_at IS NULL
     AND (hold_until IS NULL OR hold_until < now() - interval '10 minutes');
  ```

  (`hold_until IS NULL` rows predate migration 0119 and carry no
  durable ladder to destroy.)

- [ ] **Verification:** `stellarindex_anomaly_freeze_recovered_total`
  resumes climbing on the next sweep tick (within 60 s), and the
  open-row count in postgres trends back down toward the count of
  live Redis markers + live durable holds.

## Root cause analysis

For the postmortem, capture:

- The duration of the stall and the maximum open-row backlog
- Whether the underlying transport was postgres or Redis (or
  whether the goroutine itself wasn't running)
- If the goroutine wasn't running: did the aggregator restart and
  miss wiring it up? (`freezeRecovery` block in
  `cmd/stellarindex-aggregator/main.go`.)
- Did the explorer `/anomalies` timeline visibly diverge from
  reality during the stall? Customer impact?

## Related

- [anomaly-freeze-engaged](anomaly-freeze-engaged.md) — the
  upstream runbook for the freeze itself.
- [anomaly-freeze-sustained](anomaly-freeze-sustained.md) — when a
  legitimate freeze persists past its expected window.
- `internal/aggregate/freeze/lifecycle.go` — the ADR-0019 hold /
  extension-ladder policy; `recovery.go` — the worker this runbook
  covers; `migrations/0119_freeze_events_lifecycle.up.sql` — the
  durable ladder.
- [ADR-0019](../../adr/0019-anomaly-response-and-confidence-scoring.md) —
  the policy this runbook serves.

## Changelog

- 2026-08-29 — re-verified against HEAD: alert name
  (`stellarindex_anomaly_freeze_recovery_stalled`, `severity: ticket`,
  both rule trees) + real expr (`outcome!="ok"` rate clause, `for: 2h`);
  symptom query rewritten from the forever-latching `max_over_time`
  counter form; post-0119 marker/TTL model (TTL = remaining hold +
  FreezeTTL; FreezeTTL is the silence grace; 30/10/30-min holds);
  recovery semantics (ladder-gated close, `held_by_ladder` after a
  Redis flush) + decision-tree row 3; dropped the Debug-log liveness
  check (invisible at info level, and zero-open-row sweeps log
  nothing) for the sweep-counter check; manual sweep rewritten —
  `recovered_at_ledger = 0` violated the 0018 CHECK, the 15-minute
  cutoff predates 0119's 30-min+ holds, and blanket closes destroy the
  ladder's Redis-flush protection; `stellarindex-ops freeze-unfreeze`
  as the preferred operator path; unit names (`redis-server`,
  `postgresql@15-main`) + r1 command shapes.
- 2026-05-12 — initial draft alongside the recovery worker (F-1229).
