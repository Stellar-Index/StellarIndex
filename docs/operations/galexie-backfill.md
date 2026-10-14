---
title: Galexie backfill
last_verified: 2026-10-05
status: current
---

# Galexie backfill — genesis → live-bucket handoff

One-shot replay of pubnet history into the `galexie-archive` MinIO bucket, plus the verification that proves the bytes are canonical. Steady state: [archive-completeness.md](archive-completeness.md) / [ADR-0017](../adr/0017-archive-completeness-invariants.md).

> **"Genesis → live bucket" is the GREENFIELD shape, not r1's.** ADR-0027 trimmed r1's `galexie-archive` to a **hot floor** (`stellarindex_archive_hot_floor`, rendered in `/etc/default/galexie-archive-fill`; after a trim the floor is the higher of that and `/var/lib/galexie-archive/hot-floor`). On r1:
>
> - Do **not** run this with `--start 2`: it re-downloads ~48 M ledgers the trim removed.
> - A gap **below** the floor is not a defect; rehydrate per [lcm-cache-tiering.md](lcm-cache-tiering.md).
> - A gap **above** the floor is handled by `galexie-archive-fill` and the ADR-0017 daemon.
>
> On a trimmed node substitute the hot floor for ledger 2 everywhere below.

`galexie-archive` is the immutable half, `[1, first-live-ledger − 1]`; never write history into `galexie-live`, which tails the tip.

## Backfill procedure

1. **Disk**: `data` zpool needs ~2.5 TB after zstd (`zfs list data/minio`, `zpool list`).
2. **MinIO user**: create `galexie-backfill-writer` scoped to write `galexie-archive` (`galexie-writer` covers `galexie-live` only); delete it when the job exits clean.
3. **Captive-core config**: copy `/etc/stellar/captive-core-galexie.cfg` to `captive-core-galexie-backfill.cfg`; set `CATCHUP_COMPLETE = true`, leave `CATCHUP_RECENT` unset, use a distinct `PEER_PORT`.
4. **Galexie config**: copy `/etc/galexie/galexie.toml` to `galexie-backfill.toml`; set `datastore_config.params.destination_bucket_path = "galexie-archive/"` and point `captive_core_toml_path` at the new cfg.
5. **Launch, under the heavy-job wrapper on r1**:
   ```sh
   /usr/local/sbin/run-heavy-job.sh galexie-backfill-<start>-<end> \
     /usr/local/bin/galexie scan-and-fill --start <start> --end <end>
   ```
   `--start` is 2 greenfield, the hot floor on a trimmed node; `--end` is `first-live-ledger − 1`. `scan-and-fill` is idempotent: restart freely. The wrapper is mandatory ([maintainer-workflow.md](maintainer-workflow.md) §Heavy one-shot jobs); reuse the SAME job name each attempt (a held lock exits 75).
6. **Throughput**: full pubnet ≈ 62 M ledgers, **8-14 h**. Monitor with `ssh -t r1 'watch -c -n 5 galexie-backfill-status'` or `mc du local/galexie-archive`. Phases: history download into `/srv/history-archive`, bucket apply, then ledger replay + upload (~62 M objects).

## Verification tiers

Run after a clean exit; tiers are independent.

- **Tier A — chain-link integrity (mandatory).** Asserts each `LedgerHeader.LedgerHash` equals the next ledger's `PreviousLedgerHash`: `stellarindex-ops verify-archive -tier chain`
- **Tier B — checkpoint anchoring (mandatory).** Our `LedgerHash` must match the SCP-agreed `currentLedger.hash` in each 64-ledger `/srv/history-archive/history/.../history-XXXXXXXX.json`: `stellarindex-ops verify-archive -tier checkpoint`
- **Tier C — byte-compare sample against the SDF dataset (optional).** Proves the "re-seed from SDF" DR path: `stellarindex-ops verify-archive -tier sdf-sample -from N -to M -sdf-samples 1000` (needs an explicit `-to`; not part of `-tier all`).
- **Tier D — multi-peer checkpoint cross-validation (optional, strongest).** Diff `currentLedger.hash` for ~20 sampled checkpoints across several tier-1 validators' archives (URLs are in the `[[VALIDATORS]]` blocks of `/etc/stellar/captive-core-galexie.cfg`): `stellarindex-ops verify-archive -tier peers -peer-samples 20 -peers <url>,<url>,...` (empty `-peers` uses a built-in set).
- **Tier E — stellar-archivist scan (housekeeping, operator-run only).** r1's local archive is trimmed, so a local scan fails by construction; scan a peer with a longer timeout: `stellarindex-ops verify-archive -config /etc/stellarindex.toml -tier archivist -archivist-timeout 48h -archivist-url https://history.stellar.org/prd/core-live/core_live_001`

Routine use: A + B at backfill; C once as a DR rehearsal; D quarterly or on a reported divergence.

## Tuning — when 60 ledgers/sec isn't enough

The bottleneck is galexie's single-goroutine S3 PUT loop (~60 ledgers/s); no `--workers` flag exists.

### Parallel `scan-and-fill` processes

`IfNoneMatch: "*"` makes overlapping writes idempotent, so processes on **disjoint** ranges need no coordination. Each needs its own captive-core `storage_path` and `admin_port`. Eight workers give ~470 ledgers/s (~1.5 days for pubnet instead of ~12):

```sh
for i in 1 2 3 4 5 6 7 8; do   # mkdir -p /var/galexie/$i/captive-core first
  systemd-run --unit=galexie-bf-$i galexie scan-and-fill \
    --config-file /etc/galexie/galexie-backfill-$i.toml \
    --start $(( 2 + (i - 1) * 7781216 )) --end $(( 1 + i * 7781216 ))
done
```

Each `galexie-backfill-$i.toml` sets `[stellar_core_config] storage_path = "/var/galexie/$i/captive-core"` and `admin_port = 11725 + i`.

`[datastore_config.schema] ledgers_per_file` is locked once the bucket manifest exists (greenfield only); 1 → 64 divides the PUT count by 64. Upload concurrency and zstd level are hard-coded.

### Highest-impact lever (in practice): mirror from the AWS public bucket

AWS hosts a free, anonymous-read, genesis-complete galexie-format dataset at `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` (~2.5 TB, ~9 h on r1). For historical backfill skip the export and `mc mirror` it into `galexie-archive`: new node, DR on a corrupt bucket, or mid-backfill cutover. Trade-off: no cross-validation against your own export; if audit matters, run `scan-and-fill` once and Tier-C compare.

#### Runbook — fresh bucket (greenfield)

```sh
sudo systemctl stop galexie-backfill   # stop any in-flight scan-and-fill
mc alias set aws https://s3.us-east-2.amazonaws.com "" "" --api S3v4
# --skip-errors, NOT --overwrite=false (see "mc mirror gotcha")
mc mirror --skip-errors \
  aws/aws-public-blockchain/v1.1/stellar/ledgers/pubnet/ \
  local/galexie-archive/
mc ls local/galexie-archive/FFFFFFFF--0-63999/ | head
set -a; source /etc/default/stellarindex-ops; set +a
stellarindex-ops verify-archive -config /etc/stellarindex.toml \
  -tier all -from 2 -to <last-mirrored-ledger>
```

Source `/etc/default/stellarindex-ops` first, or verify-archive gets a 403 from MinIO.

#### Runbook — recovering from a partial / mc-cp-poisoned bucket

If `galexie-archive` has objects whose mtimes differ from AWS's (typically after `mc cp`),
bucket-level `mc mirror` deadlocks. Use the per-partition script, which audits the bucket,
deletes partials and mirrors only missing or just-deleted partitions:

```sh
galexie-archive-fill          # /usr/local/bin on r1; restartable
```

Logs to `/var/log/galexie-mirror.log`.

- **Auto-partial detection**: each run re-mirrors any of the latest `PARTIAL_CHECK_WINDOW=4` AWS partitions that has fewer local files (`PARTIAL_CHECK_WINDOW=N` overrides; `0` disables).
- **Known partials outside that window**: `PARTIALS="PART1 PART2" galexie-archive-fill`, full partition names like `FC42F7FF--62720000-62783999`; any bad entry rejects the whole list (exit 1) before deleting.
- **Credentials**: the mirror runs as a bucket-scoped writer that cannot delete; partial deletes use `ARCHIVE_DELETE_ALIAS` (`local`, MinIO root), and unset it exits 1 before deleting. A manual run during the timer's run exits 75 untouched.

#### `mc mirror` gotcha — `--overwrite=false` doesn't mean what it says

`--overwrite=false` does NOT skip present objects: every dest object whose mtime differs (every `mc cp`-uploaded one) errors, and after ~120 K errors the worker pool deadlocks silently. `--skip-errors` is fine for **fresh** buckets only. On pre-existing `mc cp` content use the per-partition script.

#### Antipattern: do not run `mc cp --recursive` in xargs against galexie-archive

It is not resumable, hides partials from partition-level worklists, and poisons later `mc mirror` runs with current-time mtimes. To stop one, SIGTERM the **outer bash** (killing xargs alone gets respawned), let orphan `mc cp` workers drain, then run `galexie-archive-fill`.

## State after backfill completes

`galexie-archive/` holds 1 → `first-live-ledger − 1`; `galexie-live/` the rest. `galexie-backfill-writer` is **deleted** once the job exits clean.

## Hazard — test-net captive-core must not use the PUBNET validator set

`stellar-core.cfg.j2` emits `[[HOME_DOMAINS]]` / `[[VALIDATORS]]` only when
`stellar_network == 'pubnet'`, and `configs/ansible/roles/archival-node/tasks/07-galexie.yml`
removes the pubnet-only `galexie-archive-fill` from other networks. Check early that the *"Selected archive …"*
log lines name the right network's archives.

Test-net backfill (heavy-job caps as above):

```sh
systemd-run --unit=galexie-backfill --property=User=galexie \
  --property=EnvironmentFile=/etc/default/galexie-backfill --property=MemoryMax=8G \
  /usr/local/bin/galexie scan-and-fill --config-file /etc/galexie/galexie-backfill.toml \
    --start 2 --end <galexie_start_ledger − 1>
```

After an OOM, resume from the archive's contiguous tip, not `--start 2`: skipping existing files is not skipping existing work.

## Hazard — MinIO credential drift on any `--tags galexie` run

`--tags galexie` re-renders `/etc/default/galexie` and `/etc/default/galexie-backfill` from `galexie_s3_access_key` / `galexie_archive_s3_access_key` (vault). If the MinIO users were provisioned under other names, nothing breaks until the files re-render; then galexie crash-loops with `StatusCode: 403 ... InvalidAccessKeyId`. `--tags minio` does not repair it. Reconcile MinIO to what ansible rendered:

```sh
KEY=$(grep '^AWS_ACCESS_KEY_ID='     /etc/default/galexie | cut -d= -f2-)
SEC=$(grep '^AWS_SECRET_ACCESS_KEY=' /etc/default/galexie | cut -d= -f2-)
mc admin user add    local "$KEY" "$SEC"
mc admin policy attach local galexie-writer --user "$KEY"
mc alias set probe http://127.0.0.1:9000 "$KEY" "$SEC" && mc ls probe/galexie-live/   # verify BEFORE restart
systemctl restart galexie      # ONE restart, then watch
```

Repeat with `/etc/default/galexie-backfill` + policy `galexie-archive-writer` for `galexie-archive`.

**Check BEFORE any `--tags galexie` run.** Live creds:

```sh
K=$(grep '^AWS_ACCESS_KEY_ID='     /etc/default/galexie | cut -d= -f2-)
S=$(grep '^AWS_SECRET_ACCESS_KEY=' /etc/default/galexie | cut -d= -f2-)
mc alias set probe http://127.0.0.1:9000 "$K" "$S" && mc ls probe/galexie-live/
mc admin user list local     # does $K appear as an ACCESS KEY (col 2)?
```

> **That pre-check is not sufficient**: it shows the creds live now, not what ansible is
> about to render over them. Compare the **rendered** value, from `configs/ansible/`:
>
> ```sh
> ansible -i inventory/<net>.yml archival_nodes -m debug \
>   -a "var=galexie_s3_access_key" | tail -3
> # then confirm that exact string is an ACCESS KEY (col 2) in: mc admin user list local
> ```
>
> If absent, reconcile FIRST, then run `--tags galexie`.
