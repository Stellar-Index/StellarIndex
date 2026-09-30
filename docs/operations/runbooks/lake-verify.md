---
title: Runbook — lake-verify
last_verified: 2026-09-30
status: draft
severity: P3
---

# Runbook — `stellarindex_lake_verify_stale` / `stellarindex_lake_verify_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_lake_verify_stale`, `stellarindex_lake_verify_failed` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml`; multi-host twin in `deploy/monitoring/rules/storage.yml`. |
| Typical MTTR | 15 min to triage; a backfill is hours |
| Impact | No customer impact by itself. The raw lake is no longer proven complete, so `lake_complete` on `/v1/coverage` and any restore accepted on it are unproven until the next clean run. |

`verify-lake.timer` runs `stellarindex-ops verify-lake` daily at 08:17 UTC
(+ ≤ 10 min jitter) through `run-heavy-job.sh`, genesis (`FROM=2`) to the
contiguous tip. The verdict lands in
`/var/lib/node_exporter/textfile_collector/lake_verify.prom`, written only
after every check completed.

| Check (`check=` label) | Table(s) | Proof |
| ----- | ----- | ----- |
| `contiguity` | `ledgers` | Every sequence in range is present once. |
| `entry_changes` | `ledger_entry_changes` | Every ledger with Soroban transactions carries entry changes above the auto-detected floor. |
| `hash_chain` | `ledgers` | Each ledger's `previous_ledger_hash` equals its predecessor's hash. |
| `raw_census` | `transactions`, `operations`, `contract_events` (exact); `operation_results`, `operation_participants` (presence) | Per 1M-ledger partition, active-part rows ≥ the header counts in `ledgers`. A presence-only table is short when a partition with operations holds none of its rows. |

## Symptoms

- `_stale`: `stellarindex_lake_verify_last_run_unix` is older than 48 h, or a
  lake host (one with a schema snapshot stamp) has had no verdict for 48 h.
  The timer is not installed/enabled, every run is lock-skipped (exit 75), or
  every run crashed before writing its verdict.
- `_failed`: the last completed run counted `{{ $value }}` failures for
  `check`. `journalctl -u verify-lake -n 300` holds the gap, deficiency,
  broken-link and `SHORT` lines.

## Quick diagnosis (≤ 5 min)

```sh
systemctl list-timers verify-lake.timer
systemctl status verify-lake.service
journalctl -u verify-lake --since -50h | tail -n 80
cat /var/lib/node_exporter/textfile_collector/lake_verify.prom
```

- Timer absent: the archival-node role has not been applied since the unit
  landed. Apply with `--tags ops-jobs`.
- Journal shows `lock held` / exit 75 every day: another heavy job owns the
  lock through 08:17; move the slot in `/etc/systemd/system/verify-lake.timer`
  via the template, not by hand.
- Journal shows a ClickHouse error: fix ClickHouse first; the run leaves the
  previous verdict in place and `_stale` fires once it ages out.

## Mitigation

- [ ] `contiguity` / `hash_chain`: follow the gap lines. A missing or wrong
      ledger range is re-read from the archive:
      `stellarindex-ops ch-backfill -config PATH -from LO -to HI -bucket galexie-archive -write`,
      then `ch-gate -config PATH -from LO -to HI` over the same range.
- [ ] `entry_changes`: same `ch-backfill -bucket galexie-archive` over the
      deficient ledgers.
- [ ] `raw_census` on `transactions`, `operations`, `operation_results` or
      `contract_events`: the `SHORT` line names table and partition
      (`p` × 1,000,000 … +999,999). Re-read that range with `ch-backfill
      -bucket galexie-archive -write`, then `ch-gate` it.
- [ ] `raw_census` on `operation_participants` below the live-capture floor:
      `stellarindex-ops ch-participant-backfill -from LO -to HI -write`
      re-derives participants from `stellar.operations` in the lake.
- [ ] Whether a range goes through `ch-backfill` or a projector replay is
      decided by [the replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule).
      Never add a bespoke per-table backfill.
- [ ] Verification: `systemctl start verify-lake.service`; every
      `stellarindex_lake_verify_failures` series reads 0 and both alerts
      clear within one scrape.

## Related

- [ch-lake-backup](ch-lake-backup.md) — verify-lake is the restore acceptance gate.
- [off-site-backup-plan](../off-site-backup-plan.md)
- [alerts-catalog](../alerts-catalog.md)
