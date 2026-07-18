---
title: Off-site (S3) backup plan
last_verified: 2026-07-18
status: proposed (execute after Phase A/D)
severity: P1
---

# Off-site (S3) backup plan

Design for off-site backups of R1 — today the box is a **single point of failure with local-only backups** (pgBackRest on the same ZFS pool as the data it protects; a pool/box loss loses both). This plan puts a durable copy off-box. Execute **after** Phase A/D (the user's sequencing); the design is ready now.

## Principle: back up the SOURCE OF TRUTH, not every derivative
The tiers have very different recovery costs, so back up by criticality × re-derivability:

| Data | Size | Re-derivable? | Off-site priority |
|---|---|---|---|
| **Galexie archive (MinIO LCM)** | 5.56 TiB | Only by re-walking a Stellar full-history node (days–weeks, or impossible for pruned meta) | **🔴 CRITICAL — the root source of truth; everything else rebuilds from it** |
| **Postgres (served money state)** | 676 GiB | Yes, by re-projecting from the lake — but slow, and it holds live-authoritative state | **🔴 HIGH — native pgBackRest→S3** |
| **Config / vault / secrets / systemd** | < 1 GiB | No (secrets) | **🔴 HIGH — small, encrypted tarball** |
| **ClickHouse lake** | 8.6 TiB | **Yes — fully re-derivable from the archive** (`ch-backfill` + `verify-lake`) | **🟢 LOW — skip the bulk; document the rebuild path** |

**Key insight:** the CH lake (the largest dataset) does **not** need off-site backup — it is a *materialization* of the galexie archive. Protect the **archive** and the lake is recoverable. This cuts the off-site footprint from ~15 TiB to **~6.3 TiB**.

## Provider
Recommend **Cloudflare R2** (S3-compatible, **zero egress fees** → cheap restores/drills, and you already run Cloudflare) or **Backblaze B2**. AWS S3 works but egress makes restores costly. All three are S3-API, so the tooling below is provider-agnostic.
- Est. cost ≈ 6.3 TiB × ~$0.015/GB-mo ≈ **~$95/mo** (R2/B2); restores ~free on R2.

## The three backup streams

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

## ClickHouse: document the rebuild, don't back up the bulk
Instead of backing up 8.6 TiB, back up only the **recipe to rebuild** it (tiny): the schema DDL (`tier1_schema.sql` is already in-repo), the cursor/watermark state, and the `done-windows` state files. Recovery = provision CH → apply schema → `ch-backfill` from the (backed-up) archive → `verify-lake`/`verify-contiguity`/`reconcile-balances`. Document the RTO (re-derive is ~days for the full lake — acceptable for a total-loss scenario given the served tier restores fast from PG). *Optionally* snapshot the served-critical CH tables (`supply_flows`, current-state projections) for faster RTO — decide based on tolerance.

## Cross-cutting
- **Restore drills:** the `data/restore-drill` ZFS dataset already exists (empty). Wire a monthly job: restore the latest PG backup there + a sampled archive object + verify. An untested backup isn't a backup.
- **Monitoring:** alert on backup-age (`stellarindex_offsite_backup_stale`) per stream — a silent backup failure is the classic trap.
- **Encryption keys:** the passphrases go in the vault (and the vault itself in stream 3) — but keep a **copy of the vault passphrase off-R1** (a password manager), else an R1 loss loses the key to its own backups. This closes the loop the current setup can't.
- **Sequencing:** stand up stream 1 (archive) + stream 2 (PG) first — they're the source of truth. Then stream 3. Then enable the local pgBackRest prune (deferred from Phase A) once repo2 is proven.

## Related
Master plan `production-readiness-master-plan-2026-07-18.md` (this is a Phase F / post-D hardening item). Restore tooling: `scripts/ops/restore-drill.sh`.
