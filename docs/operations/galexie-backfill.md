---
title: Galexie backfill
last_verified: 2026-10-05
status: current
---

# Galexie backfill — genesis → live-bucket handoff

One-shot replay of pubnet history into the `galexie-archive` MinIO bucket on an
archival node, plus the verification that proves the bytes are canonical. The
steady-state story (daily cron, cross-anchor fill, dual-archive invariants) is
[archive-completeness.md](archive-completeness.md) /
[ADR-0017](../adr/0017-archive-completeness-invariants.md).

> **"Genesis → live bucket" is the GREENFIELD shape, not r1's.** ADR-0027
> trimmed r1's `galexie-archive` to a **hot floor**
> (`stellarindex_archive_hot_floor`; read the rendered value from
> `/etc/default/galexie-archive-fill`, not from this page). After the monthly
> trim timer runs, the fill floor is the higher of that value and the cutoff the
> last trim used, persisted by `compute-trim-cutoff.sh` to
> `/var/lib/galexie-archive/hot-floor`. Below the floor is intentionally absent
> locally and lives in the cold tier. On r1:
>
> - Do **not** run this with `--start 2`: it re-downloads ~48 M ledgers the trim removed.
> - A gap **below** the floor is not a defect; rehydrate per [lcm-cache-tiering.md](lcm-cache-tiering.md).
> - A gap **above** the floor is handled by `galexie-archive-fill` and the ADR-0017 daemon.
>
> On a trimmed node substitute the hot floor for ledger 2 everywhere below.

## Why a separate bucket

`galexie-live` is appended forever by `galexie.service` (one ledger / ~5 s);
mixing historical writes in races the "live bucket tails the tip" invariant the
indexer assumes. `galexie-archive` is the immutable half, `[1, first-live-ledger − 1]`,
written once by a one-shot job. The indexer reads both; they stitch at the boundary.

## Backfill procedure

1. **Disk**: `data` zpool needs ~2.5 TB for genesis-to-tip after zstd (`zfs list data/minio`, `zpool list`).
2. **MinIO user**: `galexie-writer` is scoped to `galexie-live` only. Create
   `galexie-backfill-writer` scoped to write `galexie-archive`; delete it when the job exits clean.
3. **Captive-core config**: copy `/etc/stellar/captive-core-galexie.cfg` to
   `captive-core-galexie-backfill.cfg`; set `CATCHUP_COMPLETE = true`, leave
   `CATCHUP_RECENT` unset, use a distinct `PEER_PORT`.
4. **Galexie config**: copy `/etc/galexie/galexie.toml` to `galexie-backfill.toml`; set
   `datastore_config.params.destination_bucket_path = "galexie-archive/"` and point
   `captive_core_toml_path` at the new cfg.
5. **Launch, under the heavy-job wrapper on r1**:
   ```sh
   /usr/local/sbin/run-heavy-job.sh galexie-backfill-<start>-<end> \
     /usr/local/bin/galexie scan-and-fill --start <start> --end <end>
   ```
   `--start` is 2 greenfield, the hot floor on a trimmed node; `--end` is
   `first-live-ledger − 1`. `scan-and-fill` is idempotent: interrupt and restart
   freely, it writes only missing ledgers. The wrapper is mandatory
   ([maintainer-workflow.md](maintainer-workflow.md) §Heavy one-shot jobs):
   `MemoryMax=20G`, `MemorySwapMax=0`, batch CPU/IO weights, singleton lock, disk
   watchdog. Use the SAME job name each attempt; a held lock is refused with exit
   75 (`fuser -v` on the lock file names the holder). Run from tmux or a
   `galexie-backfill.service` oneshot that calls the wrapper.
6. **Throughput**: full pubnet ≈ 62 M ledgers, **8-14 h** on r1-class hardware.
   Monitor with `ssh -t r1 'watch -c -n 5 galexie-backfill-status'`
   (`/usr/local/bin/galexie-backfill-status`), or `mc du local/galexie-archive`
   hourly. Three phases: (1) history download of checkpoint bucket files into
   `/srv/history-archive` (bounded by peer archives; per-archive 404s self-heal by
   fail-over); (2) bucket apply (brief, CPU-bound); (3) ledger replay + upload of
   `xdr.zst` objects (`Uploading FFFFFFFF--…/<seq>.xdr.zst`, zpool writes ~5-10
   MiB/s, ~62 M objects at `LedgersPerFile = 1`).

## Verification tiers

Run after a clean exit; tiers are independent.

### Tier A — Chain-link integrity (mandatory)

Asserts `ledger[N].LedgerHeader.LedgerHash == ledger[N+1].LedgerHeader.PreviousLedgerHash`
over every LCM; catches corruption, drops and replay divergence.
`stellarindex-ops verify-archive -tier chain`

### Tier B — Checkpoint anchoring (mandatory)

Every 64 ledgers `/srv/history-archive/history/.../history-XXXXXXXX.json` holds the
SCP-agreed `currentLedger.hash`; our `LedgerHash` must match at each checkpoint
(equivalent to byte-comparing SDF's bucket; inter-checkpoint content follows by
hash chaining). `stellarindex-ops verify-archive -tier checkpoint`

### Tier C — Byte-compare sample against SDF (optional)

Proves the "re-seed from SDF" DR path works and surfaces egress issues early.
`stellarindex-ops verify-archive -tier sdf-sample -from N -to M -sdf-samples 1000`
targets the AWS public dataset (`storage.s3_cold_*`), compares ETag + size (equal
ETags are equal bytes for single-part uploads), needs an explicit `-to`, and is not
part of `-tier all`. No timer yet.

### Tier D — Multi-peer checkpoint cross-validation (optional, strongest)

For ~20 sampled checkpoints (ledger 63, 1M, 5M, 10M, 20M, 40M, current − 1000, ...),
diff `currentLedger.hash` across several tier-1 validators' archives (SDF, LOBSTR,
SatoshiPay, PublicNode, Blockdaemon, Franklin Templeton; URLs are in the
`[[VALIDATORS]]` blocks of `/etc/stellar/captive-core-galexie.cfg`).
`stellarindex-ops verify-archive -tier peers -peer-samples 20 -peers <url>,<url>,...`
(empty `-peers` uses a built-in seven-peer set). ~120 tiny GETs; run on demand.

### Tier E — stellar-archivist scan of the source archive (housekeeping)

Runs `stellar-archivist scan --verify <url>` (without `--verify` it only checks
files exist): every checkpoint file present and verifying, every referenced bucket
sha256 recomputed. Defaults to `file://<archive-root>`; `-archivist-url` scans
another archive.

**Operator-run only; not scheduled.** r1's `/srv/history-archive` is trimmed to
`history/` + `ledger/` ([storage-considerations.md](../architecture/storage-considerations.md)
Move A), so a local scan fails by construction (transactions and results read as
missing, `got 0000…`); the monthly cron and its staleness alert were retired. To audit
the full archive scan a peer and budget for re-downloading it; the 30 min flag
default is too short:

```sh
stellarindex-ops verify-archive -config /etc/stellarindex.toml \
  -tier archivist -archivist-timeout 48h \
  -archivist-url https://history.stellar.org/prd/core-live/core_live_001
```

## Tuning — when 60 ledgers/sec isn't enough

Phase 3 ran at ~59 ledgers/s (galexie claims 500-1500) with captive-core at 1.5% CPU
and spare zpool and network: the bottleneck is galexie's **single-goroutine S3 PUT
loop** (~16 ms RTT x ~60 PUT/s; `stellar-galexie internal/uploader.go` `Run`,
`go-stellar-sdk support/datastore/s3.go` `putFile`). No `--workers` flag exists.

### Highest-impact lever: parallel `scan-and-fill` processes

Galexie's `IfNoneMatch: "*"` precondition makes overlapping writes idempotent, so
processes on **disjoint** ranges are safe with no coordination. Each needs its own
captive-core `storage_path` and `admin_port`. Eight workers x ~7.7M ledgers, ~470
ledgers/s, full pubnet in ~1.5 days instead of ~12 (CPU headroom is ample at 8;
measure before going past 16):

```sh
mkdir -p /var/galexie/{1,2,3,4,5,6,7,8}/captive-core
for i in 1 2 3 4 5 6 7 8; do
  start=$(( 2 + (i - 1) * 7781216 ))
  end=$((   1 +  i      * 7781216 ))
  systemd-run --unit=galexie-bf-$i \
    galexie scan-and-fill \
      --config-file /etc/galexie/galexie-backfill-$i.toml \
      --start "$start" --end "$end"
done
```

Each `galexie-backfill-$i.toml` overrides:

```toml
[stellar_core_config]
storage_path = "/var/galexie/$i/captive-core"
admin_port   = $((11725 + i))
```

### Other knobs (lower impact, mostly not exposed)

| Knob | Status | Notes |
| --- | --- | --- |
| `[datastore_config.schema] ledgers_per_file` | exposed but locked | Persisted in the bucket manifest on first write; greenfield only. 1 → 64 divides PUT count by 64. |
| `[datastore_config.schema] files_per_partition` | exposed | Layout only, no PUT-rate impact. |
| Upload concurrency | hard-coded (single goroutine) | `Uploader.Run`; a ~15-line upstream patch. |
| `uploadQueueCapacity = 128` | hard-coded | No help with a single consumer. |
| zstd level | hard-coded `SpeedDefault` (~3) | `support/compressxdr/compressor.go`. |
| S3 client tuning | hard-coded defaults | `NewS3DataStore` uses `config.LoadDefaultConfig(ctx)`, no transport overrides. |
| Multipart | irrelevant | Only objects >= 5 MB; LCM files are small. |

### Highest-impact lever (in practice): mirror from the AWS public bucket

AWS hosts a public galexie-format dataset at
`s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`. For historical backfill
skip the export and `mc mirror` it into `galexie-archive`.

- **Floor: genesis**, complete: partition `FFFFFFFF--0-63999/` holds all 63,997
  expected objects (pubnet numbering starts at 3), no gaps.
- **Egress: free.** AWS Open Data Sponsorship (`aws-pds`, no `RequesterPays`);
  anonymous `--no-sign-request` reads work. Full ~2.5 TB observed on r1 at
  ~80 MB/s sustained, about 9 h.
- **Right call**: new archival node from cold; DR on a corrupt `galexie-archive`;
  mid-backfill cutover (`IfNoneMatch` makes it idempotent against uploaded objects).
- **Trade-off**: you lose cross-validation against your own captive-core export; if
  audit matters, run `scan-and-fill` once and Tier-C compare against AWS. The
  bucket is in `us-east-2`; throughput is network-bound, small-object listings are slower.

#### Runbook — fresh bucket (greenfield)

```sh
sudo systemctl stop galexie-backfill   # stop any in-flight scan-and-fill (or kill <pid>)
mc alias set aws https://s3.us-east-2.amazonaws.com "" "" --api S3v4
# --skip-errors, NOT --overwrite=false (see "mc mirror gotcha")
mc mirror --skip-errors \
  aws/aws-public-blockchain/v1.1/stellar/ledgers/pubnet/ \
  local/galexie-archive/
mc ls local/galexie-archive/FFFFFFFF--0-63999/ | head
# Expect: FFFFFFFC--3.xdr.zst, FFFFFFFB--4.xdr.zst, FFFFFFFA--5.xdr.zst, ...
set -a; source /etc/default/stellarindex-ops; set +a
stellarindex-ops verify-archive -config /etc/stellarindex.toml \
  -tier all -from 2 -to <last-mirrored-ledger>
```

`/etc/default/stellarindex-ops` sets `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`
(what the AWS SDK consumes) plus the `STELLARINDEX_S3_*` duplicates for the config
loader. Unsourced, verify-archive falls to the default credential chain and gets a
403 from MinIO. Provisioned by the `stellarindex-reader` MinIO user
(`roles/archival-node/tasks/09-minio.yml`).

#### Runbook — recovering from a partial / mc-cp-poisoned bucket

If `galexie-archive` has objects whose mtimes differ from AWS's (typically after
someone ran `mc cp`), bucket-level `mc mirror` deadlocks. Use the per-partition
script, which audits the bucket, deletes partials and mirrors only missing or
just-deleted partitions:

```sh
galexie-archive-fill          # /usr/local/bin on r1; restartable
```

Logs to `/var/log/galexie-mirror.log`; monitor with `galexie-backfill-status`.

- **Auto-partial detection**: every run file-counts the latest
  `PARTIAL_CHECK_WINDOW=4` AWS partitions and re-mirrors any local one with fewer
  files (covers a new partition AWS published with only a few ledgers). Override
  `PARTIAL_CHECK_WINDOW=N galexie-archive-fill`; `0` disables; cost is N recursive `mc ls`.
- **Known partials outside that window** (e.g. named by `verify-archive`):
  `PARTIALS="PART1 PART2 PART3" galexie-archive-fill`. Entries must be full
  partition names like `FC42F7FF--62720000-62783999`, no trailing slash; any bad
  entry rejects the whole list (exit 1) before deleting. A non-partition name in the
  AWS listing (other than `.config.json`) is never mirrored and makes the run exit 1
  after the valid partitions finish.
- **Credentials**: the mirror runs as the bucket-scoped writer (`ARCHIVE_DEST`,
  `archivewriter/galexie-archive`), which cannot delete. Partial deletes use
  `ARCHIVE_DELETE_ALIAS` (`local`, MinIO root); unset, the run exits 1 before
  deleting. An operator-set `ARCHIVE_DEST` uses its own alias for both unless
  `ARCHIVE_DELETE_ALIAS` is also set.
- **Locking**: `/run/lock/galexie-archive-fill.lock` plus a private `mktemp -d`; a
  manual run during the timer's run exits 75 untouched. Wait for
  `galexie-archive-fill.service`, then re-run.

#### `mc mirror` gotcha — `--overwrite=false` doesn't mean what it says

Verified on `mc RELEASE.2025-08-13T08-35-41Z`: `--overwrite=false` does NOT
silently skip present objects. Every dest object whose mtime differs from source
(every `mc cp`-uploaded one) emits
`(mm-source-mtime) Overwrite not allowed for …`, and after ~120 K errors the worker
pool deadlocks in `futex_` wait: process alive, no output, no progress.
`--skip-errors` drains through the same noise without deadlocking, which is fine for
**fresh** buckets but useless on pre-existing `mc cp` content (the error storm is the
bottleneck). The only reliable recovery is the per-partition script.

#### Antipattern: do not run `mc cp --recursive` in xargs against galexie-archive

1. `mc cp` has no skip-if-exists: parallel loops double GETs, a stale worklist re-fetches finished partitions.
2. Partition-level worklists hide partials: a partition holds 64 000 `.xdr.zst`
   objects, `mc ls` shows it present at 21 307, and `comm -23 aws.txt local.txt`
   then excludes it forever.
3. `mc cp --recursive partition/` is not resumable: a kill re-fetches all 64 000.

`mc cp` also poisons future `mc mirror` runs (current-time mtimes). To stop a running
`xargs ... mc cp --recursive ...`: SIGTERM the **outer bash** (killing xargs alone
gets respawned), wait for orphan `mc cp` workers to drain, then run `galexie-archive-fill`.

## What we do today

- **At backfill**: Tier A + Tier B (Tier E only against a full archive).
- **DR rehearsal**: Tier C once.
- **Health check**: Tier D quarterly, or when a downstream price consumer reports divergence.

## State after backfill completes

- `galexie-archive/`: 1 → `first-live-ledger − 1` (immutable); `galexie-live/`:
  `first-live-ledger` → tip (append-only); the indexer reads both.
- `galexie-writer` keeps write on `galexie-live`; `galexie-backfill-writer` is **deleted** once the job exits clean.

## RESOLVED — test-net captive-core used the PUBNET validator set

Found 2026-08-27 (futurenet backfill died at exit 3; testnet fetched PUBNET checkpoints).
Fixed: `stellar-core.cfg.j2` emits `[[HOME_DOMAINS]]` / `[[VALIDATORS]]` only when
`stellar_network == 'pubnet'` (21039639e, #203), and
`configs/ansible/roles/archival-node/tasks/07-galexie.yml:682` removes the pubnet-only
`galexie-archive-fill` from non-pubnet networks. Live `galexie.toml` was never affected
(no `captive_core_toml_path`).

Test-net backfill:

```sh
systemd-run --unit=galexie-backfill --property=User=galexie \
  --property=WorkingDirectory=/var/lib/galexie/backfill \
  --property=EnvironmentFile=/etc/default/galexie-backfill \
  --property=CPUWeight=50 --property=IOWeight=50 --property=MemoryMax=8G \
  /usr/local/bin/galexie scan-and-fill \
    --config-file /etc/galexie/galexie-backfill.toml \
    --start 2 --end <galexie_start_ledger − 1>
```

On restart, resume from the archive's contiguous tip, not `--start 2`: the 5G cap OOM'd at ledger 2,254,083 and systemd re-ran from 2, re-applying every ledger (skip-existing-files is not skip-existing-work).

Check early that the *"Selected archive …"* log lines name the right network's archives.

## Second hazard — MinIO credential drift on any `--tags galexie` run

`--tags galexie` re-renders `/etc/default/galexie` and `/etc/default/galexie-backfill`
from `galexie_s3_access_key` / `galexie_archive_s3_access_key` (vault). On both test
nets the MinIO users were provisioned under different names than the templates:

| ansible renders | MinIO had |
| --- | --- |
| `galexie-writer` (also the *policy* name) | user `galexie-live-writer`, policy `galexie-writer` |
| `galexie-archive-writer` | present, secret no longer matched |

It is latent until the env files re-render; then galexie crash-loops with
`failed to list objects in bucket 'galexie-live': ... StatusCode: 403 ... InvalidAccessKeyId`.
`--tags minio` did not repair it (`no_log` task failed). Reconcile MinIO to what
ansible rendered (`mc admin user add` updates an existing secret):

```sh
KEY=$(grep '^AWS_ACCESS_KEY_ID='     /etc/default/galexie | cut -d= -f2-)
SEC=$(grep '^AWS_SECRET_ACCESS_KEY=' /etc/default/galexie | cut -d= -f2-)
mc admin user add    local "$KEY" "$SEC"
mc admin policy attach local galexie-writer --user "$KEY"
mc alias set probe http://127.0.0.1:9000 "$KEY" "$SEC" && mc ls probe/galexie-live/   # verify BEFORE restart
systemctl restart galexie      # ONE restart, then watch
```

Repeat with `/etc/default/galexie-backfill` + policy `galexie-archive-writer` for
`galexie-archive`.

**Check BEFORE any `--tags galexie` run.** Pre-check of the live creds:

```sh
K=$(grep '^AWS_ACCESS_KEY_ID='     /etc/default/galexie | cut -d= -f2-)
S=$(grep '^AWS_SECRET_ACCESS_KEY=' /etc/default/galexie | cut -d= -f2-)
mc alias set probe http://127.0.0.1:9000 "$K" "$S" && mc ls probe/galexie-live/
mc admin user list local     # does $K appear as an ACCESS KEY (col 2)?
```

> **TRAP: that pre-check is not sufficient.** It shows the creds live now, not what
> ansible is about to render over them (si-futurenet authenticated fine, then broke).
> Compare the **rendered** value, from `configs/ansible/`:
>
> ```sh
> ansible -i inventory/<net>.yml archival_nodes -m debug \
>   -a "var=galexie_s3_access_key" | tail -3
> # then confirm that exact string is an ACCESS KEY (col 2) in: mc admin user list local
> ```
>
> If absent, reconcile FIRST, then run `--tags galexie`.

Real fix, partly done: the MinIO user tasks (`09-minio.yml`) and the env templates
(`galexie.env.j2`, `07-galexie.yml`) already read the same variables
(`galexie_s3_access_key`, `galexie_archive_s3_access_key`). Still open: align each
region's vault values with the live MinIO users, and stop naming a user after a policy.
