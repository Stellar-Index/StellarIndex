---
title: Off-site (S3) backup plan
last_verified: 2026-07-18
status: proposed (execute after Phase A/D)
severity: P1
---

# Off-site (S3) backup plan

Design for off-site backups of R1 — today the box is a **single point of failure with local-only backups** (pgBackRest on the same ZFS pool as the data it protects; a pool/box loss loses both). This plan puts a durable copy off-box. Execute **after** Phase A/D (the user's sequencing); the design is ready now.

## Principle: RTO-driven — re-derivability is a *last resort*, not a recovery plan
An earlier draft proposed skipping the CH lake (it's re-derivable from the archive). **That's rejected: the re-derive RTO is unacceptable for a production API.** Back up by *recovery-time*, not just re-derivability:

| Data | Size (post-ZSTD) | RTO if backed up | RTO if re-derived | Off-site priority |
|---|---|---|---|---|
| **Galexie archive (MinIO LCM)** | 5.56 TiB | hours | days–weeks / maybe impossible (pruned meta) | **🔴 CRITICAL — irreplaceable source of truth** |
| **ClickHouse lake** | ~7 TiB | ~2–16 h (S3 restore) | **~1–2 weeks** (full `ch-backfill` re-walk) | **🔴 HIGH — on the serving path; re-derive too slow** |
| **Postgres (served money state)** | 676 GiB | ~1–3 h (pgBackRest) | slow (re-project) | **🔴 HIGH — native pgBackRest→S3** |
| **Config / vault / secrets / systemd** | < 1 GiB | minutes | impossible (secrets) | **🔴 HIGH — encrypted tarball** |

**RTO math (why full CH backup wins):** re-deriving the lake = the same multi-day walk we've been sizing — ~63.5M ledgers × ~80 ledgers/s (observed) ≈ **~9 days for `entry_changes` alone**, ~1–2 weeks for the full set + served re-projection. Restoring ~7 TiB from S3 ≈ **~16 h @1 Gbps / ~2 h @10 Gbps**. For an explorer that serves arbitrary deep history, the whole lake is on the serving path — so **back it up in full.** Re-derive-from-archive stays documented as the *both-copies-gone* last resort.

**Footprint:** ~13.3 TiB off-site (archive + CH + PG + config) ≈ **~$200/mo** at R2 — incremental after the first sync (daily deltas are ~10–20 GiB). The ~$130/mo delta over the archive-only design buys a ~half-day RTO instead of ~2 weeks.

> **The better long-term answer is a warm standby, not just backups.** An S3 restore is still hours of downtime. The **HA warm standby** (Phase F — the ratified-but-undeployed patroni/CH-replica roles) replicates PG + CH continuously and fails over in **minutes**, and it eliminates the single-box SPOF entirely. Treat full S3 backup as the **interim + the "both boxes lost" floor**; the standby is the real production target.

## Provider
Recommend **Cloudflare R2** (S3-compatible, **zero egress fees** → cheap restores/drills, and you already run Cloudflare) or **Backblaze B2**. AWS S3 works but egress makes restores costly. All three are S3-API, so the tooling below is provider-agnostic.
- Est. cost ≈ 13.3 TiB × ~$0.015/GB-mo ≈ **~$200/mo** (R2/B2); restores ~free on R2.

## The four backup streams

### 1. Galexie archive → S3 (critical) — continuous mirror
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
Then `pgbackrest backup` writes both repos. **This also lets us safely prune repo1 (local) diffs** (the deferred Phase A step) once repo2 exists — the off-site copy becomes the deep-retention tier.

### 3. Config / vault / secrets → encrypted tarball (high)
Small, high-value, non-re-derivable. A daily job tars `/etc/stellarindex*`, `/etc/pgbackrest*`, systemd units, the ansible vault, and CH/PG DDL snapshots; `age`/`gpg`-encrypts; uploads to S3. Codify as a systemd timer in the archival-node role.

### 4. ClickHouse lake → S3 (high) — full, incremental, for RTO
Back up the full lake so recovery is a **restore (~hours)**, not a re-walk (~weeks).
- Tool: **`clickhouse-backup`** (S3-native, part-level **incremental** — after the first ~7 TiB full, dailies are only the new parts ~10–20 GiB). It freezes parts for a consistent snapshot; no downtime.
- Do the **first full backup AFTER the Phase A ZSTD recompress** (backs up ~7 TiB not ~8.6, and the parts are already in their final codec).
- Restore = provision CH → `clickhouse-backup restore_remote <name>` → `verify-lake`/`verify-contiguity`/`reconcile-balances` as the acceptance gate.
- **Also keep the rebuild recipe** (schema DDL, cursor/watermark, `done-windows`) — that's the *both-copies-gone* fallback: re-derive from the archive. Belt and suspenders, tiny to store.

## Cross-cutting
- **Restore drills:** the `data/restore-drill` ZFS dataset already exists (empty). Wire a monthly job: restore the latest PG backup there + a sampled archive object + verify. An untested backup isn't a backup.
- **Monitoring:** alert on backup-age (`stellarindex_offsite_backup_stale`) per stream — a silent backup failure is the classic trap.
- **Encryption keys:** the passphrases go in the vault (and the vault itself in stream 3) — but keep a **copy of the vault passphrase off-R1** (a password manager), else an R1 loss loses the key to its own backups. This closes the loop the current setup can't.
- **Sequencing:** archive (1) + PG (2) first — the irreplaceable/authoritative data. Then the CH full backup (4) — after the Phase A recompress, so it captures ~7 TiB not ~8.6. Then config (3). Then enable the local pgBackRest prune (deferred from Phase A) once `repo2` (PG off-site) is proven.
- **Restore drill must include CH now** (not just PG): a periodic `clickhouse-backup restore_remote` of a sampled table into a scratch instance + `verify-contiguity`, so the ~half-day RTO is *proven*, not assumed.

## Related
Master plan `production-readiness-master-plan-2026-07-18.md` (this is a Phase F / post-D hardening item). Restore tooling: `scripts/ops/restore-drill.sh`.
