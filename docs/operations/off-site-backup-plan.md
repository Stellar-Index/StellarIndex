---
title: Off-site (S3) backup plan
last_verified: 2026-10-05
status: Provider is Backblaze B2 for the ClickHouse lake and Postgres; §1 (Galexie archive mirror) retired, the archive is re-pulled from SDF's public bucket; §2 (Postgres repo2) LIVE on r1 on AWS S3, moving to B2; §4 (ClickHouse lake) mechanism committed, awaiting its B2 bucket; §3 proposed
severity: P1
---

# Off-site (S3) backup plan

r1 is a single point of failure: pgBackRest repo1 sits on the same ZFS pool as the data it protects. This plan puts a durable copy off-box. The long-term answer is the HA warm standby ([`../architecture/ha-plan.md`](../architecture/ha-plan.md#23-planned-regions) §2.3, ADR-0050), which fails over in minutes; off-site backup is the interim and the "both boxes lost" floor.

## Principle: RTO-driven — re-derivability is a *last resort*, not a recovery plan

Back up by recovery time, not by re-derivability:

| Data | Size off-site (measured on r1, 2026-09-28) | RTO if backed up | RTO if re-derived | Off-site |
|---|---|---|---|---|
| **Galexie archive (MinIO LCM)** | ≈ 3.1 TB (2.8 TiB) | — | days–weeks (re-pull) | **Not backed up** — a copy of `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`, re-pulled from there (ADR-0043 §2) |
| **ClickHouse lake** | **14.6 TiB (≈ 16.1 TB)**, growing ≈ 11 GiB/day | ~4–36 h (restore) | **~1–2 weeks** (full `ch-backfill` re-walk) | **HIGH** — on the serving path, B2 (§4) |
| **Postgres (served money state)** | repo2 ≈ 2.5 TiB measured (252 GiB backups + 2.2 TiB WAL; `configs/ansible/roles/archival-node/templates/pgbackrest.conf.j2`) | ~1–3 h (pgBackRest) | slow (re-project) | **HIGH** — pgBackRest repo2, B2 (§2) |
| **Config / vault / secrets / systemd** | < 1 GiB | minutes | impossible (secrets) | **HIGH** — encrypted tarball (§3) |

**RTO math:** re-deriving the lake ≈ 63.5M ledgers × ~80 ledgers/s ≈ **~9 days for `entry_changes` alone**, ~1–2 weeks for the full set plus served re-projection. Restoring ≈ 16.1 TB ≈ **~36 h @1 Gbps / ~4 h @10 Gbps**. The whole lake serves deep history, so back it up in full; re-derive-from-archive stays the both-copies-gone fallback.

**Footprint:** ≈ 18.9 TB at rest (lake 16.1 TB + repo2 2.75 TB + config), incremental after the first sync. **Peak ≈ 35 TB** while a rolling lake full runs: `ch-lake-backup.sh` removes the previous chain only after the new full is `BACKUP_CREATED`, so two lake fulls coexist — and on B2 the removed chain stays billed until the lifecycle rule purges it (see [Provider](#provider)). Size every figure from `system.parts.bytes_on_disk` / `zfs get logicalused`; ZFS `used` (9.42 T for the lake) is compressed and understates an upload by ≈ 1.45×.

## Failure ladder

Targets: **4 h to serving**, **24 h to full history** (fallback if unreachable: 24 h / 72 h). "Not measured" means no drill behind the number.

| Tier | Lost | Restores from (today) | Time | Runbook |
|---|---|---|---|---|
| 1. Process or service down | A unit, data intact | Restart in place | minutes (estimate) | [`postgres.md`](runbooks/postgres.md#stellarindex_timescale_primary_down) §B |
| 2. Bad write or deletion | Recent data on one dataset | Local ZFS snapshot (Postgres 7 d, ClickHouse 3 d), then pgBackRest repo1 | not measured | [`zfs-snapshots.md`](runbooks/zfs-snapshots.md) |
| 3. Postgres data dir lost | Served tier | pgBackRest repo1 (local); repo2 (AWS S3 eu-central-1 today, moving to B2) if the box is also gone | repo1 ~9-10 min; repo2 ~47-49 min, restore only, excluding WAL replay (measured, drills 2026-09-03 and 2026-09-15) | [`postgres.md`](runbooks/postgres.md#stellarindex_timescale_primary_down) §D, [`restore-drills.md`](drills/restore-drills.md) |
| 4. r1 lost, standby survives | Primary box | r2/r3 replica, traffic cutover | not measured (HA standby not deployed) | [`dr-activation.md`](runbooks/dr-activation.md), [`postgres.md`](runbooks/postgres.md#stellarindex_timescale_primary_down) §E |
| 5. All boxes lost | Postgres and the ClickHouse lake | Postgres from repo2; the lake has NO off-site copy yet (B2 lake copy is unfunded and not running), so history falls back to tier 6 | Postgres restore ~47-49 min (measured, excluding WAL replay); lake from a B2 copy would be ~4 h at 10 Gbps, ~36 h at 1 Gbps (estimate, once it exists) | [`ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore) |
| 6. Every copy lost | Lake and its backups | Re-ingest the raw archive from the SDF public bucket (an owned raw copy, once one exists, replaces this pull) | ~1-2 weeks (estimate, not measured) | [`galexie-backfill.md`](galexie-backfill.md), [`archive-completeness.md`](archive-completeness.md) |

Against the targets: only the Postgres restore in tier 3 is measured to meet 4 h to serving (excluding WAL replay). Tiers 1 and 2 are likely but unmeasured, and tier 4 is unknown because the standby is not deployed. Today, tier 5 history falls back to tier 6 (~1-2 weeks), which misses 24 h; it can meet 24 h only once the off-site lake copy exists and the link is 10 Gbps.

## Provider

**Backblaze B2 for every off-site copy we own: the ClickHouse lake (§4) and pgBackRest repo2 (§2).** The raw Galexie archive has no off-site copy; SDF's public AWS bucket is its upstream.

- **repo2 moves from its AWS S3 bucket to a fresh B2 repo** with a new cipher pass. The AWS repo2 retires once a B2 Postgres restore test passes. Set `repo2-s3-region` to the bucket's real region, not `auto`.
- **r1 uploads with a key that lacks `deleteFiles`.** On B2 an S3 DeleteObject from such a key succeeds by writing a *hide* marker: the version stays, restorable, and billed. So `ch-lake-backup.sh`'s chain prune and orphan sweep exit 0 but free nothing. Each bucket needs a lifecycle rule to purge hidden versions: ~14 d hide→delete on the B2 lake and Postgres buckets, set with the B2 master key; not in config. That window is also how long a wipe from r1 stays recoverable. Check with `b2 ls --versions --long` (column 2 = upload/hide).
- Until the B2 buckets exist, §4 stays unfunded and `stellarindex_ch_lake_backup_stale` keeps ticketing the host.

## The four backup streams

### 1. Galexie archive → S3 (critical) — continuous mirror

**Retired.** The archive is re-pulled from SDF's public bucket instead. The mechanism remains, disabled by default: `scripts/ops/galexie-archive-mirror.sh` + `galexie-archive-mirror.timer`, installed by `18-pgbackrest-backup.yml` only when `galexie_archive_mirror_enabled: true`, mirroring the local `galexie-archive` bucket to an off-site `mc` alias (`mc mirror`, verified each run with `mc mirror --dry-run`). Enabled, it needs `galexie_archive_mirror_s3_endpoint` and the vault key pair, else `stellarindex_galexie_archive_mirror_stale` tickets the host. Restore: [`runbooks/archive.md#stellarindex_galexie_archive_mirror_stale`](runbooks/archive.md#stellarindex_galexie_archive_mirror_stale).

Locally, `data/minio` is in the role's `zfs_snapshot_datasets` (7-day retention), which turns a mis-aimed `mc rm --recursive` into a `zfs clone`; it does nothing for a pool or box loss.

### 2. Postgres → pgBackRest S3 repo (high) — native, incremental, encrypted

pgBackRest writes a second repo natively: repo1 local (fast restore), repo2 off-site (durable).

```ini
# /etc/pgbackrest/pgbackrest.conf (rendered by pgbackrest.conf.j2)
repo2-type=s3
repo2-s3-endpoint=<b2-endpoint>
repo2-s3-bucket=stellarindex-pgbackup
repo2-s3-region=<bucket region>
repo2-cipher-type=aes-256-cbc          # passphrase in vault
repo2-retention-full=1                 # pgbackrest_repo2_retention_full (#298)
repo2-retention-diff=7                 # pgbackrest_repo2_retention_diff
```

**LIVE on r1 since 2026-08-29** (AWS S3 bucket): pgBackRest 2.58, WAL archiving to both repos.

> **`pgbackrest backup` does NOT write both repos.** With no `--repo` it backs up only the highest-priority repo (repo1). Only `archive-push` (WAL) and `expire` act on every repo. The nightly wrapper (`pgbackrest-backup.sh.j2`) therefore runs one `--repo=N --type=$TYPE backup` per `repoN-*` key in `pgbackrest.conf` — diff Mon–Sat, full Sunday, expire per repo against its own retention — and emits `stellarindex_pgbackrest_backup_{last_success_unix,last_rc,duration_seconds}{repo}`. By hand, add `--repo=2` for the off-site copy. Staleness alert: `stellarindex_backup_offsite_stale`.

Repo2 should let us prune repo1's local diffs (the deferred Phase A step). **Not yet true:** repo2 is lean (1 full + 7-day diffs), shallower than repo1's 2 fulls, and repo1 has not been pruned, so repo1 remains the deeper copy.

### 3. Config / vault / secrets → encrypted tarball (high)

Small, high-value, non-re-derivable. **No such job exists yet.** A daily job tars `/etc/stellarindex*`, `/etc/pgbackrest*`, systemd units, the ansible vault, and CH/PG DDL snapshots; `age`/`gpg`-encrypts; uploads off-site. Codify as a systemd timer in the archival-node role.

> **hashdb (`/var/lib/stellarindex/hashdb.bin`, ADR-0016) is deliberately out of every stream and out of `zfs_snapshot_datasets`.** It is a disposable append-log the indexer regenerates; losing it costs a re-append of the trailing `verify_window_ledgers` window (default 20000, ~1 day at 5s/ledger) on next start. Per-region switch: `stellarindex_hashdb_enabled` in `stellarindex.toml.j2`.

### 4. ClickHouse lake → S3 (high) — full, incremental, for RTO

**Mechanism committed (ADR-0043 §2.4), not yet running on r1.** `scripts/ops/ch-lake-backup.sh` + `ch-lake-backup.timer`, installed by `18-pgbackrest-backup.yml`; restore in [`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md). It backs nothing up until `ch_lake_backup_s3_endpoint` (bucket + prefix, trailing slash) and the vault key pair are set.

- ClickHouse native `BACKUP DATABASE` into the `s3_plain` disk `si_lake_backup` declared in config.d (credentials never in query text). A full every `ch_lake_backup_full_interval_days` (28), daily incrementals via `base_backup` (≈ 11 GiB/day). `ch_lake_backup_retain_chains: 1`; the old chain is removed only after the new full is `BACKUP_CREATED`. `ch_lake_backup_max_bandwidth` 100 MiB/s keeps a full off the serving disks.
- A failed full leaves a partial upload that never reaches the chain record; after each confirmed full, every unrecorded older chain directory is swept as an orphan (`stellarindex_ch_lake_backup_orphans_removed`). Exit 2 = backup succeeded but prune or sweep failed. On B2 both only hide (see [Provider](#provider)).
- Take the **first full after the capacity-relief recompress** plus a `tx_hash_index` dedupe: `operations.body_xdr` and `operation_results.result_xdr` are still LZ4 on r1 (the retired phase-a-capacity-relief runbook (git history) Step 3b) and `tx_hash_index` is 2× duplicated (an `OPTIMIZE … FINAL`, not in that runbook) — ≈ 2.8 TiB off every chain.
- Restore = provision CH → `RESTORE DATABASE … FROM Disk('si_lake_backup', …)` ([`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore)) → `verify-lake`/`verify-contiguity`/`reconcile-balances` as the acceptance gate. `verify-lake` runs daily on the live lake ([`runbooks/lake-verify.md`](runbooks/lake-verify.md)).
- Keep the rebuild recipe (schema DDL, cursor/watermark, `done-windows`) as the both-copies-gone fallback; `scripts/ops/ch-schema-snapshot.sh` (§2.1) stores it.

## Cross-cutting

- **Restore drills:** `scripts/ops/restore-drill.sh` (`restore-drill.timer` for repo1, `restore-drill-offsite.timer` for repo2) restores Postgres into scratch plus a ClickHouse re-derive sample; alerts `stellarindex_restore_drill_stale`, `stellarindex_restore_drill_offsite_stale`, `stellarindex_restore_drill_failed`.
- **Restore drill must include CH from backup:** a periodic native `RESTORE` ([`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore)) of a sampled table into a scratch instance + `verify-contiguity`, so the restore RTO is proven, not assumed. Still open.
- **Monitoring:** one staleness alert per stream — `stellarindex_backup_offsite_stale` (repo2), `stellarindex_ch_lake_backup_stale`, `stellarindex_ch_schema_snapshot_offsite_stale`.
- **Encryption keys:** passphrases live in the vault (and the vault in stream 3); keep a copy of the vault passphrase off r1 (a password manager), else an r1 loss loses the key to its own backups.
- **Sequencing:** Postgres to B2 (2), then retire the AWS repo2 after a B2 restore test passes. Then the lake full (4), after the recompress and `tx_hash_index` dedupe (≈ 11.8 TiB, not 14.6). Then config (3). Then the local repo1 prune, once repo2 is proven.

## ADR-0043 §2.3 tail insurance amendment

ADR-0043 §2.3 originally pushed the newest N days of `contract_events` + `ledgers` off-site daily. It was never implemented; the ADR now carries the amendment (tail insurance is satisfied by the §2.1 snapshot, no ClickHouse data push). The derivation:

1. **The tail is not unarchived.** `galexie-archive-fill.sh` fills `galexie-archive` from AWS (AWS partitions − local partitions, hourly), not from `galexie-live`. Recent raw LCM sits in `galexie-live` and `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` from the moment it exists. `galexie_archive_tip_lag_ledgers` measures the local mirror lagging by up to one 64,000-ledger partition (~4 days; alerted >64k, paged >128k).
2. **The tail is the most recoverable part of the chain.** Raw LCM is a deterministic re-export from a captive core replaying the public history archives; meta pruning bites old ledgers, never the newest days.
3. **Cost/benefit fails the RTO test.** ~64,000 ledgers at ~80 ledgers/s ≈ **~13 minutes** to re-walk, against ≈ 7 GiB/day of push (~116 KiB/ledger × 64k).
4. **What is irreplaceable is the bookkeeping, and §2.1 captures it daily:** `partitions.tsv`, `ch-backfill-done-windows.txt`, `ingestion-cursors.tsv` and `schema.sql` (`scripts/ops/ch-schema-snapshot.sh`).

REVISIT if (a) the lake gains a table that is not a deterministic decode of LCM (an external price tick, an operator annotation), or (b) `stellarindex_galexie_archive_tip_lag_ledgers` holds a sustained floor above one partition.

## Related

[v1-launch-plan.md](v1-launch-plan.md); ADR-0043; restore tooling `scripts/ops/restore-drill.sh`.
