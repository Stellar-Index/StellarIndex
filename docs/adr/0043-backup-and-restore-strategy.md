---
adr: 0043
title: Backup + restore strategy — offsite repo2, ClickHouse lake protection, drilled restores
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0043 — Backup + restore strategy

- **Closes (design):** CS-110 (restore never drilled), CS-111
  (backups co-located with the DB), CS-112 (ClickHouse lake has no
  backup). Operator executes; everything here is scripted/config.

## Context

- **Postgres (served tier):** pgBackRest, stanza `stellarindex`,
  WAL-archived, retention 2 fulls — but `repo1` lives in the SAME
  host MinIO/ZFS pool as the database it protects. One pool loss
  destroys primary AND every backup.
- **ClickHouse (raw lake, the ADR-0034 source of truth):** zero
  backup, not Ansible-provisioned. BUT the lake is *derivable*: it is
  a structural decode of the Galexie ledger archive, which exists in
  the local MinIO `galexie-archive` bucket AND publicly in
  `aws-public-blockchain` (ADR-0027 cold tier). The question is not
  "can we recover" but "how long" — the original full backfill was a
  multi-week job.
- **No restore of anything has ever been executed** — `pgbackrest
  info` is the only verification (CS-110), and the dr-activation
  runbook overclaimed drill status (fixed, CS-113).

## Decision

### 1. Postgres: offsite `repo2`, restore drilled monthly

- Add pgBackRest `repo2` on offsite S3-compatible object storage
  (Hetzner Storage Box or Backblaze B2 — operator picks the account;
  config vars are in the ansible role, gated on presence so the role
  is a no-op until credentials exist). Async archiving to both repos;
  retention: repo1 keeps 2 fulls (fast local restore), repo2 keeps 4
  fulls (survival copy).
- `scripts/ops/restore-drill.sh` (ships with this ADR) performs a
  NON-DESTRUCTIVE scratch restore on r1: `pgbackrest restore` into a
  throwaway data dir, start a disposable postgres on port 5499,
  verify row-count + hash-chain sanity queries against the live DB,
  report, destroy. Wired to a monthly timer once the operator has run
  it by hand twice. A backup that has never restored is a hope, not a
  backup.

### 2. ClickHouse: protect the metadata, PROVE the re-derive, back up the tail

Full CH backup (~multi-TiB, growing) is rejected as the primary
strategy: the lake's ground truth (raw LCM) already exists in two
independent archives, and paying object-storage for a third copy of
derived data is poor spend. Instead, three cheaper guarantees:

> **§2 amended 2026-08-29 (audit backup-restore-6).** "Two independent
> archives" is no longer two archives *we hold*. r1's `galexie-archive`
> was capacity-trimmed below ledger 49,984,000 on 2026-07-26 (ADR-0027
> hot floor; `galexie-archive-trim` deletes an object only after the
> matching AWS object HEADs OK), so what we hold is the genesis chunk
> `[0, 63999]` plus `[49984000, tip]`. The second archive — and the
> **only** copy of `[64000, 49983999]` (~50M ledgers, ~2.3 TiB) we can
> reach — is the **AWS Public Blockchain dataset**
> (`s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`, AWS Open
> Data Sponsorship, anonymous read). Verified 2026-08-29: 1,003
> contiguous 64,000-ledger partitions covering `0..64,191,999` (to
> tip), manifest `{ledgersPerBatch: 1, batchesPerPartition: 64000,
> compression: zstd}`, pubnet passphrase.
>
> **Decision: accept the dependency; do not duplicate the public data
> into our own storage.** The dataset is the same raw LCM SDF's
> `history.stellar.org` archives can regenerate, so its loss is a
> *time* exposure (re-export from a captive core replaying public
> history), not a *data* exposure. A ~2.3 TiB Deep Archive copy of a
> public dataset buys RTO on a program-shutdown scenario that has never
> happened, and would itself need a fill/verify pipeline.
>
> **Consequences.** (a) Deep-history re-derive (§2.2, the restore
> drill's CH half, `galexie-archive-fill`, ADR-0027 cold reads) depends
> on a third party we do not control and that carries no SLA. (b) That
> dependency is therefore **monitored, not assumed**:
> `.github/workflows/public-dataset-check.yml` lists the bucket weekly
> (no credentials, `--no-sign-request`) and asserts contiguous coverage
> `0..≥ tip − 2 partitions`, the `HEX--start-end` naming, an unchanged
> manifest, and the trimmed range `[64000, 49983999]` fully present;
> drift opens/updates a single "AWS Public Blockchain dataset drift"
> issue (decision core `scripts/ci/check-public-dataset.sh`,
> fixture-tested in CI). (c) `docs/architecture/multi-region-ha.md` §5
> still lists a full off-site raw archive incl. a one-time pull of the
> middle range as the target that closes this exposure; until that is
> funded, this monitor is the control. (d) **Explicit option, not
> taken:** if zero third-party dependence is ever wanted, a one-time
> cross-region S3 copy of the middle range into an account we own is
> ≈ $80 one-off (2.3 TiB × cross-region transfer) + ≈ $3–4/mo (S3 Deep
> Archive) — trigger it from the drift issue if the dataset ever stops
> matching the assertions above.

1. **Schema + state backup (tiny, daily):** `SHOW CREATE` DDL for
   every table + the ch-live-catchup/backfill cursor state, pushed to
   repo2 alongside pgBackRest. Losing DDL/config is what turns a
   re-derive from "run the script" into archaeology.
2. **Re-derive path is drilled, not assumed:** the restore drill's CH
   half re-derives a RANDOM 100k-ledger window into a scratch
   database via the existing `ch-backfill` machinery and reconciles
   counts against the live lake. This proves the recovery machinery
   + measures throughput, giving an honest RTO figure
   (extrapolated full-rebuild time) reported into the drill log.
3. **Tail insurance:** the newest N days of `contract_events` +
   `ledgers` (the window between Galexie-archive certification and
   live) are included in the daily offsite push — the only window
   where the lake could hold data the archives don't yet.

If the measured full-rebuild RTO exceeds what we can tolerate
post-launch (verification + explorer lake surfaces dark for that
long), REVISIT with `clickhouse-backup` incremental to offsite — the
partition scheme (1M-ledger partitions, old partitions ~immutable)
makes incrementals cheap. That decision needs the drill's throughput
number first; do not pre-buy storage on a guess.

### 3. Drill logging is append-only evidence

Every drill run appends to `docs/operations/drills/` (date, repo
used, restore duration, verification results, RTO extrapolation).
The sev-playbook's annual-DR section stays aspirational until R2/R3
exist; the monthly scratch drill is what we CAN honestly do on one
host, so it is what we commit to.

## Consequences

- A single ZFS-pool loss no longer destroys the backups with the
  database (repo2), and "restore works" becomes a measured monthly
  fact with an evidence trail.
- The CH lake's protection cost is ~GBs/day (DDL + tail) instead of
  TiBs, with the re-derive path exercised instead of trusted.
- Operator actions (accounts, credentials, first two hand-runs)
  are queued in the operator register; everything else is committed
  code/config.
