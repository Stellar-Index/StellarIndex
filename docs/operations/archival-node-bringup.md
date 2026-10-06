---
title: Archival node bring-up — end-to-end recipe
last_verified: 2026-04-27
status: living doc
---

# Archival node bring-up

Provision (or rebuild) an archival node from a reachable box to `stellarindex-indexer`
committing rows to TimescaleDB. Recovering an existing node: jump to
[§ Disaster recovery](#disaster-recovery). Otherwise read top-down.

---

## Prerequisites

| Need | Where it lives | Notes |
|---|---|---|
| Provisioned host | Hetzner / equivalent | Ubuntu 24.04+, ≥ 4 NVMe drives, ≥ 192 GB RAM (per `archival-node-spec.md`) |
| Root SSH access | `inventory/<host>.yml` (`ansible_user: root`) | Hetzner installimage default; not yet hardened |
| ansible-vault password | Operator's password manager | Never on disk |
| Inventory file | `configs/ansible/inventory/<host>.yml` | Copy from `r1.example.yml`, fill in disk serials + IP |
| Inventory secrets | `configs/ansible/inventory/<host>.secrets.yml` | `ansible-vault create` if new; needs `postgres_pass_*`, `minio_root_password`, `galexie_s3_*`, `stellarindex_reader_secret_key`, `stellarindex_pass_stellarindex` |
| Local Go ≥ 1.25 | Operator's machine | Cross-compile step in `14-stellarindex-services.yml`; check `go version` |

The role installs `stellar-archivist` (`07-galexie.yml`).

---

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

Each of the four `--extra-vars` is required; omitting one silently yields a half-built host:

- **`secrets_file=`**: the playbook loads
  `vars_files: "{{ secrets_file | default('../inventory/r1.secrets.yml') }}"`. `--extra-vars "@…"`
  does not change that default, so a non-r1 host would load **r1's** vault. Set the variable;
  do not `@`-load the file.
- **`manage_stellarindex_binaries=true`**: defaults `false` (`14-stellarindex-services.yml:46,62,107`);
  a greenfield host has no binaries.
- **`stellarindex_apply_migrations=true`**: defaults `false` (`:197`); otherwise the database is empty.
- **`run_clickhouse=true`**: defaults `false` (`defaults/main.yml:82`) while
  `storage.clickhouse_live_sink` defaults `true` (`internal/config/config.go:937`,
  `stellarindex.toml.j2`). The indexer returns `clickhouse live-sink: …` at boot if it cannot
  connect (`cmd/stellarindex-indexer/main.go`), so without ClickHouse it **will not start** at
  step 6. Install it, or opt out with `stellarindex_clickhouse_live_sink=false` (loses the
  certified lake, the CH completeness path and lake-derived supply; ADR-0034/0041).

Creates: ZFS pool + datasets, MinIO single-node + buckets + IAM (`galexie-writer`,
`galexie-archive-writer`, `stellarindex-reader`), Postgres 15 + TimescaleDB + `stellarindex`
db/role, galexie service (live tail starts immediately), all five `stellarindex-*` binaries
(cross-compiled locally), migrations, and the indexer unit (initially **stopped**; see step 6).

Verify:

```sh
ssh <host> 'systemctl is-active galexie minio postgresql@15-main'
# All three should print: active
ssh <host> 'mc admin user list local'
# Three users: galexie-writer, galexie-archive-writer, stellarindex-reader
ssh <host> 'mc ls local/'
# Three buckets: galexie-archive (empty), galexie-live (filling), backups
ssh <host> 'sudo -u postgres psql -d stellarindex -c "\dt"'
# trades, oracle_updates, ingestion_cursors, schema_migrations
```

**Capture the live-start ledger** (the live seam: where the archive ends and live begins):

```sh
ssh <host> 'pgrep -af "galexie append"'
# /usr/local/bin/galexie append --config-file ... --start <SEAM>
```

### 2. Mirror the SDF history archive (3–4 h wall, 7 TB)

`/srv/history-archive` is the trusted reference `verify-archive` Tier B anchors checkpoint
hashes against.

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

**Expect a fatal error count at the end** (peer 4xx/timeouts leave partial writes). Step 3 is
mandatory before `verify-archive`.

### 3. Sweep + heal `/srv/history-archive` (5–10 min sweep + 5 min refetch)

Find every corrupt `.gz`:

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

# Wait for /tmp/sweep-done to appear, then check the count:
ssh <host> 'wc -l /tmp/corrupt-gz.txt; awk -F/ "{print \$4}" /tmp/corrupt-gz.txt | sort | uniq -c'
```

Re-fetch with `stellar-archivist` (no `refetch-history-archive` helper exists). Delete first:
archivist fetches absent files only, never overwrites.

```sh
ssh <host> 'xargs -a /tmp/corrupt-gz.txt -r rm -f'
ssh <host> "systemd-run --unit=archivist-refetch --no-block \
  stellar-archivist mirror \
    https://history.stellar.org/prd/core-live/core_live_001 \
    file:///srv/history-archive"
ssh <host> 'journalctl -u archivist-refetch -f'
```

Repair anything archivist could not from the nine cross-anchor sources with
`archive-completeness`. `-to` is REQUIRED (`0` is rejected) and `-write` is REQUIRED (without
it `fix`/`verify` fail-closed DRY RUN and write nothing); see
[archive-completeness.md](archive-completeness.md):

```sh
ssh <host> '/usr/local/sbin/run-heavy-job.sh archive-completeness-bringup \
  /usr/local/bin/stellarindex-ops archive-completeness fix \
    -write -from <hot-floor-or-2> -to <tip> -workers 8 -network pubnet'
```

Sweep again to confirm:

```sh
ssh <host> 'find /srv/history-archive -type f -name "*.gz" -print0 \
  | xargs -0 -P 16 gzip -t 2>&1 | head'
# Empty output = clean.
```

### 4. Mirror the historical Galexie data (4–6 h wall, 4.8 TB)

Historical ledger-meta comes from the AWS public blockchain bucket into `galexie-archive`,
via the per-partition helper that handles the `mc mirror` mtime gotcha
([galexie-backfill.md](galexie-backfill.md)):

```sh
ssh <host> '/usr/local/bin/galexie-archive-fill 2>&1 | tee /var/log/galexie-archive-fill.log'
```

It audits local partitions, deletes partials, computes the missing-from-AWS set, and runs
`mc mirror --skip-errors` per missing partition, 8-way parallel (r1: ~4 h, ~1 500 files/s).
Confirm 974 partitions and 4.7+ TB:

```sh
ssh <host> 'mc ls local/galexie-archive/ | wc -l'
ssh <host> 'zfs list -Ho used data/minio'
```

### 5. Verify integrity (1.5–2 h wall)

```sh
ssh <host>
# Read the EnvironmentFile VERBATIM, never `source` it: values are unquoted (systemd
# style), so the shell would expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes in
# a secret (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh / compute-archive-to.sh.
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

`-tier` is single-valued (`chain | checkpoint | peers | archivist | all`; a comma list is
`unknown -tier`); `all` = chain + checkpoint + peers + archivist. The heavy-job wrapper is
mandatory for a walk this long ([maintainer-workflow.md](maintainer-workflow.md) §Heavy one-shot jobs).

`<SEAM>` is the step-1 live-start ledger. Tier A walks every ledger and checks hash-chain
links; Tier B compares every 64th ledger's hash with local `/srv/history-archive`; Tier E runs
`stellar-archivist scan` on the local archive. Tier E needs the full mirror, so `-tier all`
fails once `/srv/history-archive` is trimmed to `history/` + `ledger/`
(storage-considerations.md Move A); after the trim use `-tier chain` and `-tier checkpoint`.

Expected: `verified <N> ledgers, chain-link integrity OK ✓, checkpoint anchor OK ✓ (XX matched,
YY missed)`. **Both Tier A and Tier B must say OK.** If Tier B trips on
`archive read failed: open gz stream: EOF` or `unexpected EOF`, step 3 was incomplete:
sweep + refetch that partition's checkpoint range and resume.

### 6. Set the live seam in inventory + reapply, start the indexer (5 min)

Step 1 left the indexer stopped via `LiveSeamLedger=0` (live-only mode refuses to start
without a cursor). Set the real seam:

```yaml
# inventory/<host>.yml
stellarindex_live_seam_ledger: <SEAM>          # from step 1
stellarindex_backfill_from_ledger: 2           # genesis
stellarindex_enabled_sources:
  - soroswap
  - aquarius
  - phoenix
  # add others as their per-WASM-hash audit completes
```

```sh
ansible-playbook playbooks/archival-node.yml \
  --tags stellarindex \
  --inventory inventory/<host>.yml \
  --extra-vars "secrets_file=inventory/<host>.secrets.yml" \
  --ask-vault-pass
```

(Same `secrets_file=` caveat as step 1.) This re-templates `/etc/stellarindex.toml` and
restarts `stellarindex-indexer.service`. Expected log:

```
ledgerstream: archive phase from=2 to=<SEAM-1>
... ~hours of trade/oracle inserts ...
ledgerstream: archive phase complete; handing off to live
ledgerstream: live-only seam=<SEAM>
```

```sh
ssh <host> 'journalctl -fu stellarindex-indexer'
# In another window:
ssh <host> 'sudo -u postgres psql -d stellarindex -c "
  SELECT source, count(*), max(ts), max(ledger)
  FROM trades GROUP BY source ORDER BY 2 DESC;
"'
```

In live mode, trade rows should land within ~5 s of each ledger close.

---

## Disaster recovery

The node holds three stores, each derived from the one before: MinIO (`galexie-archive`,
`galexie-live`) is ground truth, the ClickHouse lake is decoded from it, and projected
Postgres tables are projected from the lake. Restore in that order.

### OS mirror reinstalled, data drives left intact

Before re-applying the role run `zpool list -H data`. Non-zero exit = pool not imported (no
`zpool.cache` survives an OS reinstall); `03-zfs.yml`'s create task refuses to proceed
because `zpool create -f` would overwrite the pool (all three stores). Confirm `data` is
listed with `zpool import -d /dev/disk/by-id` (read-only), then `zpool import data`. Pass
`-e zfs_data_pool_recreate_ack=true` only after confirming with `zdb -l <device>` that the
pool found is not the one you need.

### Galexie service is down

```sh
ssh <host> 'systemctl status galexie -n 50'
# Common causes: captive-core PEER_PORT collision (see r1-
# deployment-state.md "Configuration pitfalls" §1), MinIO
# unreachable, archive tip stale.
```

Most failures self-heal via `Restart=on-failure`; if it loops the journal has captive-core stderr.

### `galexie-archive` has missing or partial partitions

(e.g. stray `mc cp` partials, or objects lost to disk failure.) Symptom: verify-archive trips
on a missing or truncated `.xdr.zst`. Identify partials with the partition-counts approach in
`/usr/local/bin/galexie-archive-fill` (audit phase); for a known partial, delete and re-mirror:

```sh
ssh <host> 'PARTIALS="<partition-id>" /usr/local/bin/galexie-archive-fill'
```

Never fix a partial with `mc cp --recursive` ([galexie-backfill.md](galexie-backfill.md) "Antipattern").

### `/srv/history-archive` has corrupt files

Same as step 3: sweep, delete, re-mirror, then cross-anchor fix.

```sh
ssh <host> 'find /srv/history-archive -type f -name "*.gz" -print0 \
  | xargs -0 -P 16 gzip -t 2>&1 | sed -E "s/^gzip: //;s/:.*//" > /tmp/corrupt-gz.txt'
ssh <host> 'xargs -a /tmp/corrupt-gz.txt -r rm -f'
ssh <host> 'stellar-archivist mirror \
  https://history.stellar.org/prd/core-live/core_live_001 file:///srv/history-archive'
```

### Postgres is empty / wiped

```sh
# Re-run migrations:
ssh <host> 'while IFS= read -r l || [ -n "$l" ]; do case "$l" in [A-Za-z_]*=*) export "$l";; esac; done < /etc/default/stellarindex-ops; \
  stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up'
```

The ingestion cursor is gone: set `stellarindex_backfill_from_ledger: 2` in inventory,
re-apply, and watch the archive phase replay. That replay walks galexie-archive through the
dispatcher, so it refills only dispatcher-written (non-projected) domains (`sdex`, `band`,
supply observers), with no AWS round-trip; wall-clock ≈ the first-bring-up archive phase.

Projected domains (Soroban-venue `trades`, reflector/redstone `oracle_updates`, `sep41_*`,
`blend_*`, the rest of `internal/pipeline/sink.go::IsProjectedEvent`) do **not** come from that
replay. The projector reads the ClickHouse `contract_events` lake by default
(`storage.clickhouse_projector_source`); its cursors were in Postgres, so each source re-seeds
at its genesis floor and re-projects. This works only if the lake survived: check with
`stellarindex-ops verify-lake` / `verify-contiguity` before calling the served tier whole,
else do [ClickHouse lake lost or damaged](#clickhouse-lake-lost-or-damaged) first. The live
projector's catch-up is capped at ~720k ledgers/hour; for a genesis-deep refill use
`projected-rebuild`, per
[the replay decision rule](../architecture/ingest-pipeline.md#the-replay-decision-rule).

### ClickHouse lake lost or damaged

Nothing else here re-derives the lake. By what survived:

1. **A lake backup chain exists.** Restore the newest link, then bring it to tip with
   `ch-live-catchup` (hours): [ch-lake-backup § Restore](runbooks/ch-lake-backup.md#restore).
2. **No chain survives.** Recreate tables from the daily schema snapshot, then re-derive from
   `galexie-archive` with `ch-full-backfill.sh` (~1–2 weeks):
   [ch-schema-restore](runbooks/ch-schema-restore.md#restore-path-snapshot--create).
   Historical `ch-backfill` reads need `-bucket galexie-archive` (it defaults to the live,
   trimmed bucket).

Either way run `stellarindex-ops verify-lake` / `verify-contiguity` before the projector reads the lake.

### MinIO data dir lost

Worst case. `galexie-live` is unrecoverable past the upstream archive horizon (the AWS bucket,
usually within ~24 h of tip); `galexie-archive` is fully recoverable via step 4.

1. Re-run the ansible role to re-template buckets + IAM.
2. Step 4 (`galexie-archive-fill`) to re-mirror from AWS.
3. Wait for galexie to fill `galexie-live`. `galexie-append.sh` resumes from the highest
   already-exported LCM + 1 (no gap); on an empty bucket it starts from SDF's
   `.well-known/stellar-history.json` archive tip minus a checkpoint margin.
4. Update `stellarindex_live_seam_ledger` if galexie restarted at a different ledger (query the new process args).
5. Re-run migrations + restart the indexer (replays from genesis per the cursor logic).

---

## Time budget summary

| Step | Wall-clock | Bottleneck |
|---|---|---|
| 1. Ansible apply | 10–15 min | apt + go cross-compile |
| 2. stellar-archivist mirror | 3–4 h | upstream bandwidth |
| 3. Sweep + refetch | 15 min | local I/O + small re-fetches |
| 4. galexie-archive-fill | 4–6 h | AWS → us-east-2 → FRA bandwidth |
| 5. verify-archive | 1.5–2 h | local datastore read |
| 6. Indexer apply + start | 5 min | systemd |
| **End-to-end** | **~10–13 h** | mostly networks |

Every step is idempotent and skips completed work; re-run on failure.

---

## Per-region variations (R2 / R3) — ⚠️ historical, ADR-0016 is superseded

> **Historical: [ADR-0016](../adr/0016-per-region-storage-strategy.md) is `Superseded`** by
> [ADR-0050](../adr/0050-multi-region-ha-architecture.md) /
> [ha-plan.md](../architecture/ha-plan.md#23-planned-regions) (2026-08-21). ADR-0050 rejects its
> Model A (Postgres replication from R1 as canonical history), the R2-on-AWS shape and the
> ClickHouse-blind sizing. **Do not provision R2 or R3 from this section**; derive the recipe
> from ADR-0050. It is kept as a record, and because §Per-region trust + verification model is
> still the shape of the Tier A/D split.

The recipe above is **R1 (Hetzner Frankfurt)**: full local mirror of every dataset. Under
ADR-0016 other regions changed these steps:

### R2 — AWS us-east-1 (galexie-direct-from-public-bucket)

R2 reads ledger-meta **directly from `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`**
(us-east-2, ~5-15 ms per GET from us-east-1, free egress), no local mirror.

- **Steps 2, 3, 4** (history-archive mirror, sweep, galexie-archive-fill): *skip*. R2 trusts
  R1's Tier B + E. The indexer reads the AWS bucket via:

  ```toml
  [storage]
  s3_endpoint        = "https://s3.us-east-2.amazonaws.com"
  s3_bucket_archive  = "aws-public-blockchain"
  s3_region          = "us-east-2"
  ```

  There is **no `s3_bucket_archive_prefix` key**: the loader is STRICT and rejects unknown keys
  (`config: unknown keys in …`, `internal/config/load.go`). `[storage]` has `s3_endpoint`,
  `s3_region`, `s3_bucket_archive`, `s3_bucket_live` only, so a bucket-internal prefix is not
  expressible; ADR-0050 must settle that before R2 is provisioned this way.

  Archive reads are anonymous (no `STELLARINDEX_S3_*` creds; galexie's S3 client falls back to
  anonymous). R2's own `galexie-live/` export still uses an authenticated us-east-1 bucket.
- **Step 5**: Tier A + D, run **twice** (`-tier` takes one value): `-tier chain` then
  `-tier peers`. A catches corruption in transit from AWS; D cross-validates against the
  built-in tier-1 peer set over HTTPS (catches a forked upstream) and needs no
  `/srv/history-archive`. ~30-45 min for both.
- **Step 6**: as R1, but `stellarindex_live_seam_ledger` is *R2's own* galexie-append start.

R2 also runs the **Tier D weekly cron** (`14-stellarindex-services.yml`). End-to-end ~1–2 h
(vs ~10–13 h for R1).

### R3 — Vultr Singapore (bare-metal + Vultr Object Storage hybrid)

R3 keeps `galexie-archive` on **Vultr Object Storage** (S3-compatible, ~5-10 ms, ~$25/mo for
4.76 TB); postgres, galexie-live and OS on local NVMe.

- **Steps 2, 3**: *skip*.
- **Step 4**: runs, but writes to Vultr Object Storage (source is always the AWS public bucket):

  ```sh
  # On r3 — set Vultr Object Storage endpoint as the destination
  mc alias set vultr-objstor https://sgp1.vultrobjects.com $VULTR_S3_KEY $VULTR_S3_SECRET
  mc alias set aws-public https://s3.us-east-2.amazonaws.com "" "" --api S3v4

  ARCHIVE_DEST=vultr-objstor/galexie-archive galexie-archive-fill
  ```

  `ARCHIVE_DEST` defaults to `local/galexie-archive`, must be `<mc-alias>/<bucket>[/<prefix>]`,
  and the script refuses anything else before its first `mc` call. ~6-8 h.
- **Step 5**: `-tier chain` then `-tier peers`; no `/srv/history-archive`; ~30-45 min.
- **Step 6**: point the indexer's archive bucket at `vultr-objstor/galexie-archive`;
  `stellarindex_live_seam_ledger` is R3's own galexie-append start ledger.

Captive-core for galexie-live runs locally (~7 GB); its writes go to a small Vultr bucket or a
local single-node MinIO on NVMe (faster, easier). End-to-end ~7-9 h, mostly the AWS-read +
Vultr-write step.

### Per-region trust + verification model

Per ADR-0016 (superseded):

- **R1** is the *integrity leader*, running all four tiers on a schedule (A nightly; B, D, E weekly).
- **R2** and **R3** run Tier A + D locally (weekly cron) and trust R1 for Tier B + E.

The **cross-region CAGG consistency monitor** (ADR-0015's contract, implementation pending)
is the strongest check: it samples `(pair, window, from_ts)` triples across regions and
asserts closed-bucket VWAP rows are byte-identical. Failures are investigated immediately;
the likely cause is decoder-version drift across regions, not upstream data divergence.

---

## What this doc deliberately doesn't cover

- **Phase-3 validator activation**: `docs/architecture/infrastructure/validator-rollout.md`.
- **Per-WASM-hash decoder audit** for full historical replay:
  `docs/architecture/ingest-pipeline.md#contract-schema-evolution` (hence the conservative default
  `enabled_sources`: soroswap + aquarius + phoenix).
- **HA / multi-region failover**: `ha-plan.md`.

---

## References

- [galexie-backfill.md](galexie-backfill.md): `mc mirror` gotcha, per-partition fill helper, the antipattern.
- [r1-deployment-state.md](r1-deployment-state.md): r1 state and configuration pitfalls.
- [docs/architecture/ingest-pipeline.md](../architecture/ingest-pipeline.md): binding rules for the indexer's ingest path.
- [docs/architecture/infrastructure/archival-node-spec.md](../architecture/infrastructure/archival-node-spec.md): hardware + software baseline.
