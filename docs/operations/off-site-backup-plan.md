---
title: Off-site (S3) backup plan
last_verified: 2026-09-29
status: §2 (Postgres → pgBackRest repo2/S3) LIVE on r1 2026-08-29; §4 (ClickHouse lake) mechanism committed 2026-09-23, awaiting its off-site target; §1 mechanism committed 2026-09-28, awaiting its off-site target; §3 proposed; off-site targets decided 2026-09-27 (BX41 for §4, B2 for §1 + §2), accounts not yet provisioned
severity: P1
---

# Off-site (S3) backup plan

> ♻️ **Refined by ADR-0050 / [`../architecture/multi-region-ha.md`](../architecture/multi-region-ha.md) §5 (2026-08-21).** The plan adopts this doc's core (off-site is a P1 SPOF fix) and resolves its RTO argument: it keeps **two** off-site artifacts — the raw archive (crown-jewel source of truth) *and* a copy of the derived lake (fast-RTO restore). Its Cloudflare R2 provider choice and its 2026-08 sizes are superseded by [§Provider](#provider) and the table below.

Design for off-site backups of R1 — today the box is a **single point of failure with local-only backups** (pgBackRest on the same ZFS pool as the data it protects; a pool/box loss loses both). This plan puts a durable copy off-box. Execute **after** Phase A/D (the user's sequencing); the design is ready now.

## Principle: RTO-driven — re-derivability is a *last resort*, not a recovery plan
An earlier draft proposed skipping the CH lake (it's re-derivable from the archive). **That's rejected: the re-derive RTO is unacceptable for a production API.** Back up by *recovery-time*, not just re-derivability:

| Data | Size off-site (measured on r1, 2026-09-28) | RTO if backed up | RTO if re-derived | Off-site priority |
|---|---|---|---|---|
| **Galexie archive (MinIO LCM)** | ≈ 3.1 TB (2.8 TiB) | hours | days–weeks / maybe impossible (pruned meta) | **🔴 CRITICAL — irreplaceable source of truth** |
| **ClickHouse lake** | **14.6 TiB (≈ 16.1 TB)**, growing ≈ 11 GiB/day | ~4–36 h (restore) | **~1–2 weeks** (full `ch-backfill` re-walk) | **🔴 HIGH — on the serving path; re-derive too slow** |
| **Postgres (served money state)** | repo2 ≈ 0.5 TB (1 full + 7 d, compressed) | ~1–3 h (pgBackRest) | slow (re-project) | **🔴 HIGH — native pgBackRest→S3** |
| **Config / vault / secrets / systemd** | < 1 GiB | minutes | impossible (secrets) | **🔴 HIGH — encrypted tarball** |

**RTO math (why full CH backup wins):** re-deriving the lake = the same multi-day walk we've been sizing — ~63.5M ledgers × ~80 ledgers/s (observed) ≈ **~9 days for `entry_changes` alone**, ~1–2 weeks for the full set + served re-projection. Restoring ≈ 16.1 TB ≈ **~36 h @1 Gbps / ~4 h @10 Gbps**, link permitting. For an explorer that serves arbitrary deep history, the whole lake is on the serving path — so **back it up in full.** Re-derive-from-archive stays documented as the *both-copies-gone* last resort.

**Footprint:** ≈ **19.7 TB (≈ 17.9 TiB) at rest** off-site (lake 16.1 TB + archive 3.1 TB + repo2 0.5 TB + config), incremental after the first sync (daily lake deltas ≈ 11 GiB). **Peak ≈ 36 TB** while a rolling CH full runs: `ch-lake-backup.sh` removes the previous chain only after the new full is `BACKUP_CREATED`, so two lake fulls coexist for that window. Size every object-store figure from `system.parts.bytes_on_disk` / `zfs get logicalused` — ZFS `used` (9.42 T for the lake) is ZFS-compressed and understates what an upload sends by ≈ 1.45×.

> **The better long-term answer is a warm standby, not just backups.** An S3 restore is still hours of downtime. The **HA warm standby** (Phase F — the ratified-but-undeployed patroni/CH-replica roles) replicates PG + CH continuously and fails over in **minutes**, and it eliminates the single-box SPOF entirely. Treat full S3 backup as the **interim + the "both boxes lost" floor**; the standby is the real production target.

## Provider
**Decided 2026-09-27: split by stream** (ADR-0043 §1 leaves the account to the operator):
- **Hetzner Storage Box BX41 (20 TB, flat rate) — the ClickHouse lake (§4).** The lake is the bulk of the bytes and grows daily; a flat-rate box is cheaper for it than per-TB object storage.
- **Backblaze B2 — the Galexie archive mirror (§1) and pgBackRest repo2 (§2)**, ≈ 3.6 TB together, the smaller and egress-sensitive sets. repo2 moves to B2 from its current S3 bucket with a fresh cipher pass. Each bucket needs a 1-day hidden-version lifecycle rule (B2 keeps overwritten/deleted versions otherwise), and `repo2-s3-region` set to the bucket's real region, not `auto`.

Two constraints on the BX41 leg must be resolved before §4 can be pointed at it — they are wiring, not a change of decision:
- **No S3 API.** A Storage Box speaks SFTP/SCP, SMB, WebDAV and rsync; the shipped `ch-lake-backup.sh` writes an `s3_plain` disk (`clickhouse-lake-backup-disk.xml.j2`). Targeting the box means a ClickHouse disk on a mounted share (e.g. SMB) or a different transport — a role change, not just setting `ch_lake_backup_s3_endpoint`.
- **Capacity.** 20 TB holds one 16.1 TB chain, but not the ≈ 32 TB of two lake fulls during a rolling full. Either the retention step changes (drop the old chain before the new full, accepting a window with no complete chain) or the target needs more than 20 TB. The §4 recompress alone does not close it: two ≈ 13 TB fulls still exceed 20 TB.

Until the BX41 and the B2 account exist, every stream except §2 stays unfunded and its `*_stale` alert keeps ticketing the host.

## The four backup streams

### 1. Galexie archive → S3 (critical) — continuous mirror

> **Status (2026-09-28, NS03): mechanism COMMITTED, not yet running on r1.** The local half already landed (2026-09-19): `data/minio` is the third dataset in the role's `zfs_snapshot_datasets` (7-day retention), after it was measured on r1 at 2.64 TB holding **zero** snapshots while the ClickHouse and Postgres datasets *derived from it* held 4 and 8. That turns a mis-aimed `mc rm --recursive` into a `zfs clone`; it does nothing for a pool or box loss, which is what this section is for — the only off-site credential on r1 was pgBackRest's (scoped to its own repo2 bucket), so §2 covered Postgres and nothing else. What shipped now: `scripts/ops/galexie-archive-mirror.sh` + `galexie-archive-mirror.timer`, installed by `18-pgbackrest-backup.yml` behind `galexie_archive_mirror_enabled`, mirroring the local `galexie-archive` bucket to an off-site `mc` alias (default tool: `mc mirror`, verified each run with a post-mirror `mc mirror --dry-run`); restore in [`runbooks/galexie-archive-mirror.md`](runbooks/galexie-archive-mirror.md). It backs nothing up until `galexie_archive_mirror_s3_endpoint` and the vault key pair are set; until then `stellarindex_galexie_archive_mirror_stale` tickets the host. When costing that spend, note the honest blast radius: the archive is **reconstructible** by re-ingesting the public Stellar history archives, so losing it is a days-to-weeks recovery with no third-party SLA — expensive and reputationally bad, not permanent. The irreplaceable-forever framing in the table above overstates it.

The archive is **append-only** (historical LCM never changes), so an incremental mirror is cheap after the first sync.
- Tool: `mc mirror --watch` (MinIO's native, already installed) or `rclone sync` (crypt-wrapped). Bucket→bucket, server-side where possible.
- **Client-side encryption** (`rclone crypt` or SSE-C) — the archive is public-chain data (low secrecy) but encrypt anyway for uniform policy.
- Cadence: continuous `--watch`, or hourly `mc mirror`. Verify with periodic object-count + a sampled hash compare.

### 2. Postgres → pgBackRest S3 repo (high) — native, incremental, encrypted
pgBackRest supports a **second repo** natively — add S3 as `repo2` so every backup lands both local (fast restore) and off-site (durable):
```ini
# /etc/pgbackrest/pgbackrest.conf
repo2-type=s3
repo2-s3-endpoint=<r2-or-b2-endpoint>
repo2-s3-bucket=stellarindex-pgbackup
repo2-s3-region=auto
repo2-cipher-type=aes-256-cbc          # repo encryption (passphrase in vault)
repo2-retention-full=4
repo2-retention-diff=14
```
**Status (2026-08-29): LIVE on r1** — pgBackRest 2.58, repo1 local +
repo2 = S3 (rendered by `pgbackrest.conf.j2`, lean retention
`repo2-retention-full=1` / `repo2-retention-diff=7` per #298, stanza
upgraded, WAL archiving to both repos, first repo2 full taken by hand).

> ⚠️ **`pgbackrest backup` does NOT write both repos.** The `backup`
> command is single-repo: with no `--repo` it backs up only the
> highest-priority repo (the lowest key, repo1). Only `archive-push`
> (WAL) and `expire` operate on every repo by default (pgBackRest User
> Guide, "Multiple Repositories"; `pgbackrest help backup repo`). The
> nightly wrapper (`pgbackrest-backup.sh.j2`) therefore runs one
> `--repo=N --type=$TYPE backup` per `repoN-*` key it finds in
> `pgbackrest.conf` — repo1 then repo2, diff Mon–Sat / full Sunday on
> both, expire per repo against that repo's own retention — and emits
> `stellarindex_pgbackrest_backup_{last_success_unix,last_rc,duration_seconds}{repo}`
> textfile metrics. A hand-run `pgbackrest --stanza=stellarindex backup`
> only refreshes repo1; add `--repo=2` for the off-site copy.

This also lets us safely prune repo1 (local) diffs (the deferred Phase A step) now that repo2 exists. **Not yet true on r1**: repo2 ships lean (1 full + 7-day diffs, #298 / ADR-0043 §1 amendment), shallower than repo1's 2 fulls, and repo1 pruning has not happened — so repo2 is not currently the deep-retention tier it was meant to become. That statement only holds once repo1's diffs are actually pruned; until then repo1 stays the deeper copy.

### 3. Config / vault / secrets → encrypted tarball (high)
Small, high-value, non-re-derivable. A daily job tars `/etc/stellarindex*`, `/etc/pgbackrest*`, systemd units, the ansible vault, and CH/PG DDL snapshots; `age`/`gpg`-encrypts; uploads to S3. Codify as a systemd timer in the archival-node role.

> **hashdb (`/var/lib/stellarindex/hashdb.bin`, ADR-0016) is deliberately OUT of every stream above and out of `zfs_snapshot_datasets`.** It is a local, disposable append-log the indexer regenerates as it walks ledgers — not source data and not on the serving path. Losing it costs a re-append of the trailing `verify_window_ledgers` window (default 20000, ~1 day at 5s/ledger) on next indexer start, nothing more. It gets no off-site copy and no ZFS snapshot for the same reason `os` (its containing dataset) doesn't: none of it is worth the retention-day cost of a serving-path or archive dataset. The per-region enable lever now exists — `stellarindex_hashdb_enabled` in `stellarindex.toml.j2` — independent of this backup story.

### 4. ClickHouse lake → S3 (high) — full, incremental, for RTO
Back up the full lake so recovery is a **restore (~hours)**, not a re-walk (~weeks).

> **Status (2026-09-23, #859): mechanism COMMITTED, not yet running on r1.** ADR-0043 §2.4 adopts this stream. What shipped differs from the bullets below in one respect: it uses ClickHouse's native `BACKUP DATABASE` (full every 28 days, daily incrementals via `base_backup`) into an `s3_plain` disk declared in config.d, not `clickhouse-backup` — no third-party binary, and the credentials stay out of query text. `scripts/ops/ch-lake-backup.sh` + `ch-lake-backup.timer`, installed by `18-pgbackrest-backup.yml`; restore in [`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md). It backs nothing up until `ch_lake_backup_s3_endpoint` and the vault key pair are set; until then `stellarindex_ch_lake_backup_stale` tickets the host. The CH leg of the restore drill (last bullet of *Cross-cutting*) is still open.

- Tool: ClickHouse's native **`BACKUP DATABASE`** (part-level **incremental** via `base_backup` — after the first 14.6 TiB full, dailies are only the new parts ≈ 11 GiB). It takes a consistent snapshot of parts; no downtime.
- Do the **first full backup AFTER the capacity-relief recompress** plus a `tx_hash_index` dedupe: `operations.body_xdr` and `operation_results.result_xdr` are still LZ4 on r1 ([`runbooks/phase-a-capacity-relief-2026-07-18.md`](runbooks/phase-a-capacity-relief-2026-07-18.md) Step 3b recompresses them) and `tx_hash_index` is 2× duplicated (an `OPTIMIZE … FINAL`, not in that runbook) — ≈ 2.8 TiB reclaimable across both, off every chain the BX41 stores.
- Restore = provision CH → `RESTORE DATABASE … FROM Disk('si_lake_backup', …)` ([`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore)) → `verify-lake`/`verify-contiguity`/`reconcile-balances` as the acceptance gate.
- **Also keep the rebuild recipe** (schema DDL, cursor/watermark, `done-windows`) — that's the *both-copies-gone* fallback: re-derive from the archive. Belt and suspenders, tiny to store.

## Cross-cutting
- **Restore drills:** the `data/restore-drill` ZFS dataset already exists (empty). Wire a monthly job: restore the latest PG backup there + a sampled archive object + verify. An untested backup isn't a backup.
- **Monitoring:** alert on backup-age (`stellarindex_offsite_backup_stale`) per stream — a silent backup failure is the classic trap.
- **Encryption keys:** the passphrases go in the vault (and the vault itself in stream 3) — but keep a **copy of the vault passphrase off-R1** (a password manager), else an R1 loss loses the key to its own backups. This closes the loop the current setup can't.
- **Sequencing:** archive (1) + PG (2) first — the irreplaceable/authoritative data. Then the CH full backup (4) — after the capacity-relief recompress and the `tx_hash_index` dedupe, so it captures ≈ 11.8 TiB not 14.6. Then config (3). Then enable the local pgBackRest prune (deferred from Phase A) once `repo2` (PG off-site) is proven.
- **Restore drill must include CH now** (not just PG): a periodic native `RESTORE` ([`runbooks/ch-lake-backup.md`](runbooks/ch-lake-backup.md#restore)) of a sampled table into a scratch instance + `verify-contiguity`, so the ~half-day RTO is *proven*, not assumed.

## ADR-0043 §2.3 "tail insurance" — assessment + recommended amendment

**Status (2026-07-25 assessed, 2026-09-24 landed): §2.3 as written was
never implemented and ADR-0043 §2.3 now carries the amendment below.
§2.1 (the ClickHouse schema+state snapshot) ships and covers the part of
the tail that is genuinely irreplaceable.**

ADR-0043 §2.3 commits to:

> **Tail insurance:** the newest N days of `contract_events` + `ledgers`
> (the window between Galexie-archive certification and live) are
> included in the daily offsite push — the only window where the lake
> could hold data the archives don't yet.

Re-derived against the system as it actually stands, that premise no
longer holds, and the mitigation it prescribes fails this document's own
RTO-driven test.

**1. The window is smaller than the ADR assumed, and it is not
unarchived.** `galexie-archive-fill.sh` does not mirror
`galexie-live` → `galexie-archive`; it mirrors **AWS →
`galexie-archive`**, computing (AWS partitions − local partitions) and
pulling the difference hourly. So the raw LCM for a recent ledger sits in
*two independent places* the moment it exists: `galexie-live` on r1's
pool, and `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`
published by the AWS Open Data Sponsorship. The
`galexie_archive_tip_lag_ledgers` gap the ADR points at is the *local
durable mirror* lagging by up to one 64,000-ledger partition (~4 days at
mainnet pace, alerted at >64k / paged at >128k) — it is **not** a window
where the data exists nowhere but the lake.

**2. Beneath both, the tail is the most recoverable part of the chain,
not the least.** Raw LCM is a deterministic re-export from a captive core
replaying the public Stellar history archives. Meta pruning — the reason
`galexie-archive` is called irreplaceable in the table above — bites
*old* ledgers, never the newest four days. The tail is precisely the
window the network is guaranteed to still be able to hand back.

**3. The cost/benefit inverts this document's own principle.** The
principle above is *RTO-driven*: re-derive the full lake ≈ 1–2 weeks, so
back it up. The §2.3 tail is ~64,000 ledgers; at the observed ~80
ledgers/s re-walk rate that is **~13 minutes**, inside a host rebuild
that is already hours long. Backing it up would cost roughly 7 GiB/day of
push (~116 KiB/ledger × 64k) — on a pool that is capacity-blocked and to
an offsite target r1 does not yet have. Paying GiB/day for minutes of RTO
is the "poor spend" §2 rejects a full lake backup for, at 1/1000th the
scale.

**4. What in the tail actually *is* irreplaceable — and is now
captured.** Not the rows: the *bookkeeping*. After a pool loss you must
know how far the lake had got, which windows had been banked, and under
what schema — otherwise the re-derive is guesswork. That is exactly
`partitions.tsv`, `ch-backfill-done-windows.txt`, `ingestion-cursors.tsv`
and `schema.sql` in the §2.1 daily snapshot
(`scripts/ops/ch-schema-snapshot.sh`, shipped 2026-07-25). §2.1 is the
tail insurance that was worth buying.

**ADR-0043 amendment (landed 2026-09-24, §2.3):**

> **§2.3 amended 2026-07-25.** Tail insurance is satisfied without a
> ClickHouse data push. galexie-archive is filled from
> aws-public-blockchain, not from galexie-live, so recent raw LCM is
> independently held off-box from the moment it is published; and the
> newest ledgers are the ones the public history archives are most
> certain to still serve. The irreplaceable part of the tail — the tip,
> the banked-window set and the live DDL — is captured daily by §2.1.
> REVISIT if either (a) the lake gains a table whose contents are NOT a
> deterministic decode of LCM, or (b)
> `stellarindex_galexie_archive_tip_lag_ledgers` develops a sustained
> floor above one partition, meaning the local mirror has stopped
> converging.

Condition (a) is the one that would genuinely reopen this: today every
lake table is a structural decode of LCM (ADR-0034), so nothing in the
tail is unrecoverable. A future table sourced from a live-only feed (an
external price tick, an operator annotation) would not be, and would need
its own tail copy.

## Related
Master plan `production-readiness-master-plan-2026-07-18.md` (this is a Phase F / post-D hardening item). Restore tooling: `scripts/ops/restore-drill.sh`.
