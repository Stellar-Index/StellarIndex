---
title: Archival node bring-up — end-to-end recipe
last_verified: 2026-04-27
status: living doc
# Archival node bring-up

Provision an archival node from a reachable box to `stellarindex-indexer` committing rows to TimescaleDB. Recovering a node: [§ Disaster recovery](#disaster-recovery).

## Prerequisites

| Need | Where it lives | Notes |
|---|---|---|
| Provisioned host | Hetzner or equivalent | Ubuntu 24.04+, ≥ 4 NVMe, ≥ 192 GB RAM |
| Root SSH | `inventory/<host>.yml` (`ansible_user: root`) | |
| ansible-vault password | Password manager | Never on disk |
| Inventory file | `configs/ansible/inventory/<host>.yml` | Copy `r1.example.yml`; fill disk serials + IP |
| Inventory secrets | `configs/ansible/inventory/<host>.secrets.yml` | `ansible-vault create` if new; needs `postgres_pass_*`, `minio_root_password`, `galexie_s3_*`, `stellarindex_reader_secret_key`, `stellarindex_pass_stellarindex` |
| Local Go ≥ 1.25 | Operator's machine | Cross-compiles the binaries |

## Bring-up sequence

### 1. Apply the ansible role (10–15 min wall, mostly waits)

```sh
cd configs/ansible
ansible-playbook playbooks/archival-node.yml \
  --inventory inventory/<host>.yml \
  --extra-vars "secrets_file=inventory/<host>.secrets.yml" \
  --extra-vars "manage_stellarindex_binaries=true" \
  --extra-vars "stellarindex_apply_migrations=true" \
  --extra-vars "run_clickhouse=true" \
  --ask-vault-pass
```

All four `--extra-vars` are required; omitting one silently yields a half-built host:

- **`secrets_file=`**: the default is r1's vault, and `--extra-vars "@…"` does not change it. Do not `@`-load.
- **`manage_stellarindex_binaries=true`**: defaults `false`; else no binaries.
- **`stellarindex_apply_migrations=true`**: defaults `false`; else an empty database.
- **`run_clickhouse=true`**: defaults `false`, but the live sink defaults on, so the indexer **will not start** at step 6 without ClickHouse. Or opt out with `stellarindex_clickhouse_live_sink=false` (loses the lake; ADR-0034/0041).

Creates: ZFS, MinIO + buckets + IAM, Postgres 15 + TimescaleDB, galexie (live tail starts immediately), the five `stellarindex-*` binaries, migrations, and the indexer unit (**stopped** until step 6).

Verify:

```sh
ssh <host> 'systemctl is-active galexie minio postgresql@15-main'   # all active
ssh <host> 'mc admin user list local'   # galexie-writer, galexie-archive-writer, stellarindex-reader
ssh <host> 'mc ls local/'               # galexie-archive (empty), galexie-live (filling), backups
ssh <host> 'sudo -u postgres psql -d stellarindex -c "\dt"'   # trades, oracle_updates, ingestion_cursors, schema_migrations
```

**Capture the live seam** (where the archive ends and live begins):

```sh
```

**Capture the live seam** (where the archive ends and live begins):

```sh
ssh <host> 'pgrep -af "galexie append"'
# /usr/local/bin/galexie append --config-file ... --start <SEAM>
```

### 2. Mirror the SDF history archive (3–4 h wall, 7 TB)

`/srv/history-archive` (via `stellar-archivist`, installed by the role) is what `verify-archive` Tier B anchors against.

```sh
ssh <host>
tmux new-session -d -s archive-mirror "
  set -eux
  cd /srv/history-archive
  stellar-archivist mirror \
    https://history.stellar.org/prd/core-live/core_live_001/ \
    file:///srv/history-archive/ \
    --concurrency 64 2>&1 | tee /var/log/stellar-archivist-mirror.log
"
```

Expect a fatal error count at the end; step 3 is mandatory before `verify-archive`.

### 3. Sweep + heal `/srv/history-archive` (5–10 min sweep + 5 min refetch)

```sh
ssh <host> 'systemd-run --unit=archivist-sweep --no-block bash -c "
  find /srv/history-archive -type f -name \"*.gz\" -print0 \
    | xargs -0 -P 16 -n 50 bash -c \"
        for f in \\\$@; do
          gzip -t \\\"\\\$f\\\" 2>/dev/null || echo \\\"\\\$f\\\"
        done
      \" _ > /tmp/corrupt-gz.txt
  echo done > /tmp/sweep-done
"'

ssh <host> 'wc -l /tmp/corrupt-gz.txt'   # after /tmp/sweep-done appears
```

Delete first (archivist only fetches absent files), then re-fetch:

```sh
ssh <host> 'xargs -a /tmp/corrupt-gz.txt -r rm -f'
ssh <host> "systemd-run --unit=archivist-refetch --no-block \
  stellar-archivist mirror \
    https://history.stellar.org/prd/core-live/core_live_001 \
    file:///srv/history-archive"
ssh <host> 'journalctl -u archivist-refetch -f'
```

Repair the rest with `archive-completeness` (`-to` and `-write` REQUIRED; without `-write` it is a dry run; [archive-completeness.md](archive-completeness.md)):

```sh
ssh <host> '/usr/local/sbin/run-heavy-job.sh archive-completeness-bringup \
  /usr/local/bin/stellarindex-ops archive-completeness fix \
    -write -from <hot-floor-or-2> -to <tip> -workers 8 -network pubnet'
```

Re-run the sweep; empty output means clean.

### 4. Mirror the historical Galexie data (4–6 h wall, 4.8 TB)

Historical ledger-meta comes from the AWS public bucket ([galexie-backfill.md](galexie-backfill.md)):

```sh
ssh <host> '/usr/local/bin/galexie-archive-fill 2>&1 | tee /var/log/galexie-archive-fill.log'
```

It deletes partials and mirrors each missing partition, 8-way parallel. Expect 974 partitions, 4.7+ TB:

```sh
ssh <host> 'mc ls local/galexie-archive/ | wc -l'
ssh <host> 'zfs list -Ho used data/minio'
```

### 5. Verify integrity (1.5–2 h wall)

```sh
ssh <host>
# read the env file verbatim; never `source` it
while IFS= read -r l || [ -n "$l" ]; do
  case "$l" in [A-Za-z_]*=*) export "$l";; esac
done < /etc/default/stellarindex-ops

tmux new-window -t gbackfill -n verify-A
tmux send-keys -t gbackfill:verify-A "
/usr/local/sbin/run-heavy-job.sh verify-archive-bringup \
  /usr/local/bin/stellarindex-ops verify-archive \
    -config /etc/stellarindex.toml \
    -tier all \
    -from 2 -to <SEAM-1> \
  2>&1 | tee /var/log/galexie-verify.log
" Enter
```

`-tier` is single-valued (`chain | checkpoint | peers | archivist | all`). The heavy-job wrapper is mandatory ([maintainer-workflow.md](maintainer-workflow.md)).

`<SEAM>` is the step-1 live-start ledger. Tier A checks hash-chain links; Tier B compares every 64th ledger's hash with `/srv/history-archive`; Tier E needs the full mirror, so once it is trimmed to `history/` + `ledger/` use `-tier chain` and `-tier checkpoint`.

Expected: `verified <N> ledgers, chain-link integrity OK ✓, checkpoint anchor OK ✓`; both must say OK. A Tier B `open gz stream: EOF` or `unexpected EOF` means step 3 was incomplete: sweep and refetch that checkpoint range, then resume.

### 6. Set the live seam in inventory + reapply, start the indexer (5 min)

Set the real seam (the indexer is stopped):

```yaml
# inventory/<host>.yml
stellarindex_live_seam_ledger: <SEAM>          # from step 1
stellarindex_backfill_from_ledger: 2           # genesis
stellarindex_enabled_sources:
  - soroswap
  - aquarius
  - phoenix
  # add others after their per-WASM-hash audit
```

```sh
ansible-playbook playbooks/archival-node.yml \
  --tags stellarindex \
  --inventory inventory/<host>.yml \
  --extra-vars "secrets_file=inventory/<host>.secrets.yml" \
  --ask-vault-pass
```

This re-templates the config and restarts the indexer. Log:

```
ledgerstream: archive phase from=2 to=<SEAM-1>
ledgerstream: archive phase complete; handing off to live
ledgerstream: live-only seam=<SEAM>
```

```sh
ssh <host> 'journalctl -fu stellarindex-indexer'
ssh <host> 'sudo -u postgres psql -d stellarindex -c "
  SELECT source, count(*), max(ts), max(ledger)
  FROM trades GROUP BY source ORDER BY 2 DESC;
"'
```

In live mode, rows land within ~5 s of ledger close.

## Disaster recovery

Three stores, each derived from the one before: MinIO (ground truth), the ClickHouse lake, projected Postgres tables. Restore in that order.

### OS mirror reinstalled, data drives left intact

Before re-applying the role run `zpool list -H data`; non-zero exit = pool not imported (`03-zfs.yml` refuses to create over it). Run `zpool import -d /dev/disk/by-id` (read-only), then `zpool import data`. Pass `-e zfs_data_pool_recreate_ack=true` only after `zdb -l <device>` shows the pool found is not the one you need.

### Galexie service is down

`systemctl status galexie -n 50`. Causes: captive-core PEER_PORT collision (r1-deployment-state.md), MinIO unreachable, stale archive tip.

### `galexie-archive` has missing or partial partitions

verify-archive trips on a missing or truncated `.xdr.zst`. Delete and re-mirror the partition:

```sh
ssh <host> 'PARTIALS="<partition-id>" /usr/local/bin/galexie-archive-fill'
```

Never `mc cp --recursive` ([galexie-backfill.md](galexie-backfill.md) "Antipattern").

### `/srv/history-archive` has corrupt files

Repeat step 3: sweep, delete, re-fetch, then `archive-completeness fix`.

### Postgres is empty / wiped

```sh
# Re-run migrations:
ssh <host> 'while IFS= read -r l || [ -n "$l" ]; do case "$l" in [A-Za-z_]*=*) export "$l";; esac; done < /etc/default/stellarindex-ops; \
  stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up'
```

The ingestion cursor is gone: set `stellarindex_backfill_from_ledger: 2`, re-apply, and watch the archive phase replay. It runs through the dispatcher, so it refills only non-projected domains (`sdex`, `band`, supply).

Projected domains (`IsProjectedEvent`) come from the projector, reading the lake; its cursors were in Postgres, so each source re-seeds at genesis. That needs the lake intact: check `verify-lake` / `verify-contiguity`, else do [ClickHouse lake lost or damaged](#clickhouse-lake-lost-or-damaged) first. Genesis-deep refill: `projected-rebuild`, per [the replay decision rule](../architecture/ingest-pipeline.md#the-replay-decision-rule).

### ClickHouse lake lost or damaged

By what survived:

1. **A backup chain exists.** Restore the newest link, then catch up with `ch-live-catchup` (hours): [ch-lake-backup § Restore](runbooks/ch-lake-backup.md#restore).
2. **No chain.** Recreate tables from the schema snapshot, re-derive from `galexie-archive` with `ch-full-backfill.sh` (~1-2 weeks): [ch-schema-restore](runbooks/ch-schema-restore.md#restore-path-snapshot-to-create). `ch-backfill` historical reads need `-bucket galexie-archive`.

Either way run `verify-lake` / `verify-contiguity` before the projector reads the lake.

### MinIO data dir lost

`galexie-live` is unrecoverable past the upstream horizon (~24 h of tip); `galexie-archive` is recoverable via step 4.

1. Re-run the ansible role (buckets + IAM).
2. Step 4 to re-mirror from AWS.
3. Wait for galexie to fill `galexie-live` (it resumes from the highest exported ledger).
4. Update `stellarindex_live_seam_ledger` if galexie restarted at a different ledger.
5. Re-run migrations + restart the indexer .

## Time budget summary

| Step | Wall-clock |
|---|---|
| 1. Ansible apply | 10–15 min |
| 2. stellar-archivist mirror | 3–4 h |
| 3. Sweep + refetch | 15 min |
| 4. galexie-archive-fill | 4–6 h |
| 5. verify-archive | 1.5–2 h |
| 6. Indexer apply + start | 5 min |
| **End-to-end** | **~10–13 h** |

Every step is idempotent; re-run on failure.

## Per-region variations (R2 / R3) — ⚠️ historical, ADR-0016 is superseded

> **Historical:** [ADR-0016](../adr/0016-per-region-storage-strategy.md) is `Superseded` by [ADR-0050](../adr/0050-multi-region-ha-architecture.md) / [ha-plan.md](../architecture/ha-plan.md#23-planned-regions). **Do not provision R2 or R3 from this section**; the trust model below still describes the Tier A/D split.

### Per-region trust + verification model

Per ADR-0016: R1 is the integrity leader (all four tiers: A nightly; B, D, E weekly); R2 and R3 run Tier A + D weekly and trust R1 for Tier B + E.

The cross-region CAGG consistency monitor (ADR-0015, pending) asserts closed-bucket VWAP rows match across regions; failure usually means decoder drift.

## References

- [galexie-backfill.md](galexie-backfill.md): `mc mirror` gotcha, fill helper, antipattern.
- [r1-deployment-state.md](r1-deployment-state.md): r1 configuration pitfalls.
- [ingest-pipeline.md](../architecture/ingest-pipeline.md): ingest rules.
- [archival-node-spec.md](../architecture/infrastructure/archival-node-spec.md): hardware + software baseline.
- Out of scope: validator rollout (`docs/architecture/infrastructure/validator-rollout.md`), HA failover (`ha-plan.md`).
