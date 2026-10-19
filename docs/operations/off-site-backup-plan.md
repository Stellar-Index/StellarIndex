---
title: Off-site (S3) backup plan
last_verified: 2026-10-05
status: Provider is Backblaze B2 for the ClickHouse lake and Postgres; §1 (Galexie archive mirror) retired, the archive is re-pulled from SDF's public bucket; §2 (Postgres repo2) LIVE on r1 on AWS S3, moving to B2; §4 (ClickHouse lake) mechanism committed, awaiting its B2 bucket; §3 proposed
severity: P1
---

# Off-site (S3) backup plan

r1 is a single point of failure: pgBackRest repo1 shares a ZFS pool with the data it protects. The long-term answer is the HA warm standby ([`../architecture/ha-plan.md`](../architecture/ha-plan.md#23-planned-regions) §2.3, ADR-0050); off-site backup is the interim and the "both boxes lost" floor.

## Principle: RTO-driven — re-derivability is a *last resort*, not a recovery plan

| Data | Size (r1, 2026-09-28) | RTO if backed up | RTO if re-derived | Off-site |
|---|---|---|---|---|
| **Galexie archive (MinIO LCM)** | ≈ 3.1 TB | — | days–weeks (re-pull) | **Not backed up**; re-pulled from `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` (ADR-0043 §2) |
| **ClickHouse lake** | **14.6 TiB (≈ 16.1 TB)**, growing ≈ 11 GiB/day | ~4–36 h (restore) | **~1–2 weeks** (full `ch-backfill` re-walk) | **HIGH** — on the serving path, B2 (§4) |
| **Postgres (served money state)** | repo2 ≈ 2.5 TiB measured (252 GiB backups + 2.2 TiB WAL) | ~1–3 h (pgBackRest) | slow (re-project) | **HIGH** — pgBackRest repo2, B2 (§2) |
| **Config / vault / secrets** | < 1 GiB | minutes | impossible | **HIGH** — encrypted tarball (§3) |

**RTO math:** re-deriving the lake ≈ 63.5M ledgers × ~80/s ≈ **~9 days for `entry_changes` alone**, ~1–2 weeks in full; restoring ≈ 16.1 TB ≈ **~36 h @1 Gbps / ~4 h @10 Gbps**. Back up the whole lake.

**Footprint:** ≈ 18.9 TB at rest (lake 16.1 + repo2 2.75 + config); **peak ≈ 35 TB** while two lake fulls coexist (the old chain is removed only after the new full is `BACKUP_CREATED`; on B2 it stays billed until the lifecycle rule purges it, see [Provider](#provider)). Size from `system.parts.bytes_on_disk` / `zfs get logicalused` (ZFS `used` understates by ≈ 1.45×).

## Failure ladder

Targets: **4 h to serving**, **24 h to full history** (fallback 24 h / 72 h).

| Tier | Lost | Restores from (today) | Time | Runbook |
|---|---|---|---|---|
| 1-2. Service down; bad write | A unit; recent data on one dataset | Restart; ZFS snapshot (Postgres 7 d, ClickHouse 3 d), then repo1 | minutes (estimate); not measured | [`postgres.md`](runbooks/postgres.md#stellarindex_timescale_primary_down) §B, [`zfs-snapshots.md`](runbooks/zfs-snapshots.md) |
| 3. Postgres data dir lost | Served tier | repo1; repo2 (B2) if the box is gone | repo1 ~9-12 min; repo2 1.6-2.7 h, excluding WAL replay (measured, October drills) | [`postgres.md`](runbooks/postgres.md#stellarindex_timescale_primary_down) §D, [`restore-drills.md`](drills/restore-drills.md) |
| 4. r1 lost, standby survives | Primary box | r2/r3 replica cutover | not measured (standby not deployed) | [`dr-activation.md`](runbooks/dr-activation.md), [`postgres.md`](runbooks/postgres.md#stellarindex_timescale_primary_down) §E |
| 5. All boxes lost | Postgres and lake | Postgres from repo2; NO off-site lake copy yet, so history falls to tier 6 | Postgres 1.6-2.7 h (measured); B2 lake copy ~4 h @10 Gbps, ~36 h @1 Gbps (estimate) | [`ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore) |
| 6. Every copy lost | Lake and its backups | Re-ingest raw archive from the SDF public bucket | ~1-2 weeks (estimate) | [`galexie-backfill.md`](galexie-backfill.md), [`archive-completeness.md`](archive-completeness.md) |

Only the tier 3 Postgres restore is measured to meet 4 h. Tier 5 history falls to tier 6 (~1-2 weeks), missing 24 h until the off-site lake copy exists and the link is 10 Gbps.

## Provider

**Backblaze B2 for every off-site copy we own: lake (§4) and pgBackRest repo2 (§2).**

- **repo2 moves from AWS S3 to a fresh B2 repo** with a new cipher pass; the AWS repo2 retires once a B2 restore test passes. Set `repo2-s3-region` to the real region, not `auto`.
- **r1 uploads with a key that lacks `deleteFiles`.** On B2 an S3 DeleteObject from such a key writes a *hide* marker: the version stays, restorable and billed, so `ch-lake-backup.sh`'s chain prune and orphan sweep exit 0 but free nothing. Each bucket needs a lifecycle rule purging hidden versions (~14 d hide→delete on the lake and Postgres buckets, set with the B2 master key; not in config). That window is also how long a wipe from r1 stays recoverable. Check: `b2 ls --versions --long` (column 2 = upload/hide).
- Until the B2 buckets exist, `stellarindex_ch_lake_backup_stale` keeps ticketing the host.

## The four backup streams

### 1. Galexie archive → S3 (critical) — continuous mirror

**Retired**; mechanism kept, off by default (`galexie_archive_mirror_enabled: true` installs `scripts/ops/galexie-archive-mirror.sh` + its timer via `18-pgbackrest-backup.yml`; needs `galexie_archive_mirror_s3_endpoint` and the vault key pair, else `stellarindex_galexie_archive_mirror_stale` tickets the host). Restore: [`runbooks/archive.md#stellarindex_galexie_archive_mirror_stale`](runbooks/archive.md#stellarindex_galexie_archive_mirror_stale). `data/minio` is in `zfs_snapshot_datasets` (7 days).

### 2. Postgres → pgBackRest S3 repo (high) — native, incremental, encrypted

repo1 local (fast restore), repo2 off-site (durable), rendered by `pgbackrest.conf.j2`: `repo2-type=s3`, `repo2-s3-endpoint=<b2-endpoint>`, `repo2-s3-bucket=stellarindex-pgbackup`, `repo2-s3-region=<bucket region>`, `repo2-cipher-type=aes-256-cbc` (passphrase in vault), `repo2-retention-full=1` (`pgbackrest_repo2_retention_full`), `repo2-retention-diff=7` (`pgbackrest_repo2_retention_diff`).

**LIVE on r1 since 2026-08-29** (AWS S3): pgBackRest 2.58, WAL to both repos.

> **`pgbackrest backup` does NOT write both repos.** With no `--repo` it backs up only repo1; only `archive-push` (WAL) and `expire` act on every repo. The nightly wrapper (`pgbackrest-backup.sh.j2`) runs one `--repo=N --type=$TYPE backup` per `repoN-*` key — diff Mon–Sat, full Sunday, expire per repo —. By hand, add `--repo=2`. Staleness alert: `stellarindex_backup_offsite_stale`.

Pruning repo1's diffs is deferred: repo2 is shallower (1 full + 7-day diffs vs 2 fulls), so repo1 stays the deeper copy.

### 3. Config / vault / secrets → encrypted tarball (high)

`config-backup.timer` (daily 03:55 UTC) runs `scripts/ops/config-backup.sh`: tars `/etc/stellarindex*`, `/etc/pgbackrest*`, PostgreSQL, ClickHouse, MinIO, Galexie, Caddy and systemd config plus a `pg_dump --schema-only`, streams it straight into `age` (no plaintext on disk), keeps `config_backup_retain_days` (30) locally, and pushes to `config_backup_mc_target` (default `<ch_schema_snapshot_mc_target>/config-backup`), verifying the remote size. Recipient: `config_backup_age_recipient`; the identity is held offline, never on a host. Alerts: `stellarindex_config_backup_stale` (36 h), `_offsite_stale` (72 h).

Restore: `mc cp <target>/config-<stamp>.tar.gz.age .`, then `age -d -i <identity> config-<stamp>.tar.gz.age | tar -xzf - -C <dir>`; copy files into place by hand, never extract over `/`.

> **hashdb (`/var/lib/stellarindex/hashdb.bin`, ADR-0016) is deliberately outside every stream:** a disposable append-log the indexer regenerates. Switch: `stellarindex_hashdb_enabled`.

### 4. ClickHouse lake → S3 (high) — full, incremental, for RTO

**Mechanism committed (ADR-0043 §2.4), not yet running on r1.** `scripts/ops/ch-lake-backup.sh` + `ch-lake-backup.timer`, installed by `18-pgbackrest-backup.yml`. Inert until `ch_lake_backup_s3_endpoint` (bucket + prefix, trailing slash) and the vault key pair are set.

- Native `BACKUP DATABASE` into the `s3_plain` disk `si_lake_backup` (config.d). A full every `ch_lake_backup_full_interval_days` (28), daily incrementals via `base_backup` (≈ 11 GiB/day), `ch_lake_backup_retain_chains: 1`, `ch_lake_backup_max_bandwidth` 100 MiB/s.
- After each confirmed full, unrecorded older chain directories are swept as orphans (`stellarindex_ch_lake_backup_orphans_removed`). Exit 2 = backup ok, prune or sweep failed; on B2 both only hide.
- Take the **first full after the capacity-relief recompress** plus a `tx_hash_index` dedupe (`OPTIMIZE … FINAL`): both are still undone on r1 and cost ≈ 2.8 TiB per chain.
- Restore = provision CH → `RESTORE DATABASE … FROM Disk('si_lake_backup', …)` ([`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore)) → `verify-lake`/`verify-contiguity`/`reconcile-balances` as the gate ([`runbooks/lake-verify.md`](runbooks/lake-verify.md)).
- The rebuild recipe (schema DDL, cursor/watermark, `done-windows`) is the both-copies-gone fallback; `scripts/ops/ch-schema-snapshot.sh` (§2.1) stores it.

## Cross-cutting

- **Restore drills:** `scripts/ops/restore-drill.sh` (`restore-drill.timer` repo1, `restore-drill-offsite.timer` repo2); alerts `stellarindex_restore_drill_{stale,offsite_stale,failed}`.
- **Still open:** periodic native `RESTORE` of a sampled table into scratch + `verify-contiguity`.
- **Keys:** passphrases live in the vault; keep the vault passphrase off r1.
- **Sequencing:** Postgres to B2 (2), retire the AWS repo2 after a B2 restore test; then the lake full (4) after the recompress and dedupe (≈ 11.8 TiB, not 14.6); repo1 prune last.

## ADR-0043 §2.3 tail insurance amendment

No newest-days push of `contract_events` + `ledgers`: recent raw LCM is in `galexie-live` and the SDF public bucket at once, and re-walking ~64,000 ledgers takes ~13 minutes. The §2.1 snapshot (`partitions.tsv`, `ch-backfill-done-windows.txt`, `ingestion-cursors.tsv`, `schema.sql`, via `scripts/ops/ch-schema-snapshot.sh`) is the insurance. REVISIT if the lake gains a table that is not a deterministic decode of LCM, or `stellarindex_galexie_archive_tip_lag_ledgers` holds a floor above one 64,000-ledger partition.

## Related

[v1-launch-plan.md](v1-launch-plan.md); ADR-0043.
