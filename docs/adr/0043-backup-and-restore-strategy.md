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
