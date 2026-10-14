---
title: Runbook — lake-verify
last_verified: 2026-10-05
status: draft
severity: P3
---

# Runbook — `stellarindex_lake_verify_stale` / `stellarindex_lake_verify_failed`

## At a glance

Lake verification run alerts and re-run procedure.

P3 (`severity: ticket`). Rules: `configs/prometheus/rules.r1/storage.yml` (twin `deploy/monitoring/rules/storage.yml`). No customer impact by itself: the raw lake is unproven complete, so `lake_complete` on `/v1/coverage` and any restore accepted on it are unproven until the next clean run.

`verify-lake.timer` runs `stellarindex-ops verify-lake` daily at 08:17 UTC (+ up to 10 min jitter) via `run-heavy-job.sh`, genesis (`FROM=2`) to the contiguous tip. Verdict: `/var/lib/node_exporter/textfile_collector/lake_verify.prom`, written only after every check completed.

| Check (`check=`) | Table(s) | Proof |
| ----- | ----- | ----- |
| `contiguity` | `ledgers` | Every sequence present once. |
| `entry_changes` | `ledger_entry_changes` | Every ledger with Soroban transactions has entry changes above the auto-detected floor. |
| `hash_chain` | `ledgers` | `previous_ledger_hash` equals the predecessor's hash. |
| `raw_census` | `transactions`, `operations`, `contract_events` (exact); `operation_results`, `operation_participants` (presence) | Per 1M-ledger partition, active rows >= header counts in `ledgers`. |

## Check

```sh
systemctl list-timers verify-lake.timer
systemctl status verify-lake.service
journalctl -u verify-lake --since -50h | tail -n 80
cat /var/lib/node_exporter/textfile_collector/lake_verify.prom
```

`journalctl -u verify-lake -n 300` holds the gap, deficiency, broken-link and `SHORT` lines.

## Likely causes

- `_stale` (no verdict in 48 h): timer absent (apply the archival-node role with `--tags ops-jobs`), every run lock-skipped (exit 75: another heavy job holds the lock through 08:17; move the slot in the template, not `/etc/systemd/system/verify-lake.timer` by hand), or ClickHouse errors (fix first; the old verdict stays until it ages out).
- `_failed`: a gap, deficiency, broken link or short partition in the lake.

## Fix

1. `contiguity` / `hash_chain` / `entry_changes`: re-read the range, then gate it: `stellarindex-ops ch-backfill -config PATH -from LO -to HI -write`, `ch-gate -config PATH -from LO -to HI`. Omit `-bucket` for ledgers in the live window; add `-bucket galexie-archive` only for older ranges the trimmer removed.
2. `raw_census` on `transactions`, `operations`, `operation_results` or `contract_events`: the `SHORT` line names table and partition (`p` x 1,000,000 ... +999,999); `ch-backfill -write` that range, then `ch-gate`.
3. `raw_census` on `operation_participants` below the live-capture floor: `stellarindex-ops ch-participant-backfill -from LO -to HI -write`.
4. By hand, `stellarindex-ops` gets 403 AccessDenied on the MinIO read unless the transient unit loads the env files: `systemd-run --unit=NAME --collect -p EnvironmentFile=-/etc/default/stellarindex -p EnvironmentFile=-/etc/default/stellarindex-ops /usr/local/sbin/run-heavy-job.sh NAME /usr/local/bin/stellarindex-ops SUBCOMMAND ...`
5. Backfill vs projector replay: [the replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule); never add a bespoke per-table backfill. Verify: `systemctl start verify-lake.service`; every `stellarindex_lake_verify_failures` series reads 0.

## Re-check just the hash chain

Read-only; exit code = number of broken links (capped at 255). Run `verify-contiguity` first to tell a missing ledger from a wrong hash.

```sh
stellarindex-ops verify-hashchain -config /etc/stellarindex.toml -from <ledger> -to <ledger>
```

## Related

[ch-lake-backup](ch-lake-backup.md) (verify-lake is the restore acceptance gate), [off-site-backup-plan](../off-site-backup-plan.md), [alerts-catalog](../alerts-catalog.md)
