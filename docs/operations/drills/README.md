---
title: SEV drill framework
last_verified: 2026-10-05
status: ratified
---

# SEV drill framework

Coverage matrix item **#20 (SEV-1/SEV-2 dry-run)** is met by a three-tier
cadence pinned in [sev-playbook.md §8](../sev-playbook.md):

| Tier | Cadence | Duration | Touches systems? | Output |
| --- | --- | --- | --- | --- |
| Monthly tabletop | every month | ~30 min | no | writeup + log row |
| Quarterly chaos | every quarter | ~2 h | yes (staging only) | writeup + log row |
| Annual DR | every year | ~4 h | yes (production failover, ~1 h; protocol in [sev-playbook.md §8.3](../sev-playbook.md) and [ha-plan.md §10](../../architecture/ha-plan.md#10-roadmap-and-launch-checklist)) | writeup + log row |

Action items from a drill go to the issue tracker (label `drill-action`)
with owner and due date, and feed back into the playbook and runbooks.
Restore drills are a separate series: [restore-drills.md](restore-drills.md).

## Scenarios

Canonical scripts in `scenarios/`, re-used across drills. A cycle should
cover storage HA, cache HA, ingest robustness and aggregator anomaly
response.

| Script | Tier covered | Exercises runbook | Infra deployed today |
| --- | --- | --- | --- |
| [sev1-timescale-primary-failover](scenarios/sev1-timescale-primary-failover.md) | storage, disk-full | `timescale-primary-down.md` | yes (single node) |
| [sev1-patroni-failover](scenarios/sev1-patroni-failover.md) | storage, Patroni failover | `timescale-primary-down.md`, `replica-lag.md` | no: unvalidated draft |
| [sev1-anomaly-freeze-stuck](scenarios/sev1-anomaly-freeze-stuck.md) | aggregator | `anomaly.md#stellarindex_anomaly_freeze_engaged` | yes |
| [sev2-source-decoder-regression](scenarios/sev2-source-decoder-regression.md) | ingest | `decode-errors.md` | yes |
| [sev2-redis-sentinel-failover](scenarios/sev2-redis-sentinel-failover.md) | cache, master swap | `cache.md` | no: role exists (ADR-0024), not deployed |

## Tabletop protocol (monthly)

Tests the playbook, not the systems. ~30 min, 3 people minimum (oncall,
commander, scribe).

1. Pre-read (5 min): leader picks a script; participants skim it and its runbook.
2. Setup (2 min): leader reads *Initial conditions* and *Trigger*; scribe clones
   [`_template.md`](_template.md) to `<YYYY-MM>-<short-name>.md`.
3. Walk-through (15-20 min): leader narrates the *Injection timeline* one beat at a
   time; participants say what they would do. Scribe records gaps: runbook said X
   vs what was said, "where is that runbook", "we don't have that command or dashboard".
4. Validation (3-5 min): leader reads the pass criteria; team scores them.
5. Within 24 h: file the writeup, open `drill-action` issues, add a row to the log below.

## Chaos protocol (quarterly)

Same shape, but the trigger is really injected, **in staging only** (with synthetic
load), pre-announced at T-7d / T-1h / T-0 / resolution. The script's timeline says
how to inject (pod delete, iptables block, etc.).

## Drill log

One line per drill. Writeups are retired into this table once their open items
are tracked; a new drill keeps its writeup file until then.

| Date | Tier | Scenario | Outcome |
| --- | --- | --- | --- |
| 2026-04-30 | SEV-1 tabletop | Timescale primary failover (disk-full) | Overall pass, solo (1 participant, not the 3-person minimum). Simulated T+0:30 ack, T+5:00 root cause, T+18:00 mitigated, T+22:00 5xx under 0.5%. Scores 7 pass, 1 partial (criterion 3, `/v1/readyz` ordering; fixed). |
| 2026-04-30 | SEV-2 tabletop | Soroswap decoder regression after protocol upgrade | Overall pass, solo. Simulated T+2:30 ack, T+5:00 pattern recognised, T+10:00 decoder-side confirmed, T+15:00 status *Degraded*, T+24h backfill. 8 of 8 criteria pass. |
| 2026-06-13 | SEV-1 / SEV-2 live | API outage on r1, latency tabletop | Pass; see [incidents/sev-drill-2026-06-13.md](../incidents/sev-drill-2026-06-13.md). Detection 90 s against a 15 min target. |

### Open items from the 2026-04-30 drills

- 3-person tabletop after launch with the next on-call hire, due 2026-Q3 (inventory INV-1220; blocked on staffing).
- 3-person SEV-2 tabletop with status-page state-transition rehearsal (*Degraded, Identified, Mitigated, Operational*), due 2026-Q3 (INV-1224; same blocker).
- Per-source decode-error alert: done (INV-1221), shipped as `stellarindex_projector_decode_error_rate_high` (`deploy/monitoring/rules/projector.yml:316`, >0.1/s per source for 15m, ticket). It is a rate, not a ratio; the 5% `decode_errors / events_total` form was not built.
- stellar-core / developers.stellar.org release-notes watcher (INV-1222, discarded in the inventory; no watcher exists).
- Wire a per-source `-source` flag for `stellarindex-ops backfill` (INV-1223, discarded: bespoke backfills were removed by ADR-0032; projected sources recover with `projector-replay`, see [decode-errors.md](../runbooks/decode-errors.md)).

Done in the drill PRs: `timescale-primary-down.md` quick-diagnosis leads with `/v1/readyz`;
sev-playbook §5.3 internal-channel template cross-linked from its mitigation;
`decode-errors.md` mitigation notes elevated `flags.divergence_warning` when
`stellarindex_aggregator_class_drop_spike` fires; Patroni scenario drafted.
Withdrawn: the quarterly drill that runs `drop_chunks` on staging. The old SEV-1
mitigation (`drop_chunks('prices_1m', '30 days')`, ~120 GB freed) is no longer
sanctioned: [db-disk-full.md](../runbooks/db-disk-full.md) forbids `drop_chunks` on
data tables and disk relief is pool-level ([infra.md#stellarindex_zfs_pool_low_space](../runbooks/infra.md#stellarindex_zfs_pool_low_space)).

## Writeups

Shape in [`_template.md`](_template.md): Trigger, Response narrative, Gaps observed,
Action items, Score against the script's pass criteria. Same discipline as a
postmortem ([sev-playbook.md §6](../sev-playbook.md)), shorter.

## References

[sev-playbook.md](../sev-playbook.md) (the procedure drills exercise);
[Coverage matrix F3.5–F3.6](../../architecture/coverage-matrix.md);
[SRE workbook, postmortem culture](https://sre.google/workbook/postmortem-culture/).
