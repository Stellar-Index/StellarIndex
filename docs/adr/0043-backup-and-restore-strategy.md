---
adr: 0043
title: Backup + restore strategy — offsite repo2, ClickHouse lake protection, drilled restores
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0043: Backup + restore strategy

## Context

Postgres backups lived in the same pool as the database, the ClickHouse lake had none, and no restore had ever been run.
The lake is a deterministic decode of the Galexie ledger archive, so the question is how long recovery takes, not whether it is possible.

## Decision

1. **Postgres.** pgBackRest archives to `repo1` (local, 2 fulls) and to an offsite S3-compatible `repo2` (1 full plus 7 days of diffs and WAL, a deliberate cost choice, so repo2's horizon is about 7 days). The role does nothing until offsite credentials exist. `scripts/ops/restore-drill.sh` restores into a throwaway data dir on port 5499, checks a 100k-ledger hash-chain tail and a trades window against the live DB, and is destroyed afterward. `restore-drill.timer` runs it monthly against repo1 and `restore-drill-offsite.timer` monthly against repo2.
2. **ClickHouse lake.**
   - 2.1 A daily schema and state snapshot (`scripts/ops/ch-schema-snapshot.sh`: DDL, cursors) goes offsite with the pgBackRest push.
   - 2.2 The re-derive path is drilled, not assumed. With `DRILL_CH_WINDOW` set the drill runs a `ch-backfill` dry run (fetch and decode only) over that window and extrapolates a full-rebuild time. Only the repo1 timer sets it.
   - 2.3 No daily push of recent `contract_events` or `ledgers` rows exists. Recent raw ledgers are held off-box by the archive fill from the public dataset, and 2.1 captures what is not derivable. Revisit if a lake table stops being a deterministic decode of the ledgers, or if `galexie_archive_tip_lag_ledgers` holds a floor above one partition.
   - 2.4 The lake data backup is ClickHouse's native `BACKUP DATABASE` into an offsite `s3_plain` disk (`scripts/ops/ch-lake-backup.sh`, daily `ch-lake-backup.timer`): a full every 28 days with daily incrementals, the old chain removed only after the new full reports `BACKUP_CREATED`. It is inert until `ch_lake_backup_s3_endpoint` and keys are set, and until then `stellarindex_ch_lake_backup_stale` fires. Re-derive remains the recovery when no chain survives; the measured re-derive rate (~80 ledgers/s, about 9 days for `ledger_entry_changes` alone) is why 2.4 exists.
   - The only copy we can reach of ledgers `[64000, 49983999]` is the AWS Public Blockchain dataset (anonymous read). We accept that dependency and monitor it weekly in `.github/workflows/public-dataset-check.yml`; drift opens one issue. The untaken option, if zero third-party dependence is wanted, is a one-time cross-region copy of that range into an account we own.
3. **Drill evidence is append-only.** Each drill appends its date, repo, restore time, checks and RTO extrapolation to `docs/operations/drills/`.

## Invariant

- The drill's `ch-backfill` invocation matches that command's real flags, and its failure paths end non-zero instead of silently: `scripts/ops/restore-drill-test.sh`, `scripts/ops/restore-drill-run-test.sh`.
- The previous lake backup chain is removed only after the new full is confirmed: `scripts/ops/ch-lake-backup-test.sh`.
- An unconfigured lake backup is a visible alert, not silence: `stellarindex_ch_lake_backup_stale` in `configs/prometheus/rules.r1/storage.yml`.
- The public raw-ledger dataset keeps its assumed shape and covers the trimmed range: `scripts/ci/check-public-dataset.sh`.

## Consequences

- One pool loss no longer destroys the backups with the database, and "restore works" is a monthly measurement.
- Recovery older than repo2's 7-day window must use repo1; Postgres has no deeper offsite copy.
- Deep-history re-derive depends on a third-party dataset with no SLA.
- Operator actions (accounts, credentials, first hand runs) sit outside the repo.

## Evidence

`configs/ansible/roles/archival-node/templates/pgbackrest.conf.j2`, `scripts/ops/restore-drill.sh`, `scripts/ops/ch-lake-backup.sh`, `scripts/ops/ch-schema-snapshot.sh`, `docs/operations/runbooks/ch-lake-backup.md`, `docs/operations/off-site-backup-plan.md`.
