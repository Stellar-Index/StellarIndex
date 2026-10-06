---
title: Full Archival Node — Hardware & Software Spec
last_verified: 2026-10-06
status: reference — the per-node hardware baseline the `configs/ansible/roles/archival-node/` role assumes; r1 (Hetzner FSN1) is the deployed instance, see [r1-deployment-state.md](../../operations/r1-deployment-state.md)
---

# Full Archival Node — Hardware & Software Spec

One node runs every Stellar Index component for one region: Galexie (with
captive stellar-core as its subprocess), MinIO, ClickHouse, TimescaleDB,
Redis and the `stellarindex-*` binaries. There is no standalone
`stellar-core` service and no `stellar-rpc` on a production node (AGENTS.md
invariant 6). What runs where, and how regions relate, is in
[ha-plan.md](../ha-plan.md) §2. Hosting is rented bare metal
([ADR-0008](../../adr/0008-ha-topology.md), ADR-0050 §4), not owned colo.

A node can later be promoted to a voting validator by adding HSM-backed
keys; that sequence is [validator-rollout.md](validator-rollout.md).

---

## 3. Hardware spec — per node

### 3.1 CPU

- x86-64 with an ECC-capable platform (r1: Intel Core Ultra 7 265, 192 GB
  DDR5 ECC). stellar-core's upstream binaries target it; the role's
  preflight refuses anything else.
- ≥ 16 cores, ≥ 2.4 GHz base. Catchup is latency-bound per ledger, so
  clock beats core count up to ~16 cores.
- AES-NI, AVX2 and SHA extensions (stellar-core uses all three).

### 3.2 Memory

- **32 GB ECC is the pubnet minimum** (`archival_node_min_ram_mb`, checked
  by `tasks/01-preflight.yml`); a low-traffic test net overrides it lower in
  inventory. Postgres memory settings in `templates/postgresql.conf.j2` are
  sized against this baseline.
- ECC is mandatory: bit-flips over a multi-year, multi-TB archive are
  certain without it.
- r1 runs 192 GB, which leaves room for ClickHouse merges beside captive
  core and Postgres.

### 3.3 Storage

- **NVMe only.** Catchup is dominated by small synchronous writes; SATA
  SSDs stretch it from hours to days, and rotational disks are a false
  economy.
- **ZFS** for block checksums (bit-rot on the archive), cheap snapshots
  before upgrades, and native compression.
- **Why not raidz3:** raidz2 already tolerates two simultaneous drive
  failures, and on a 4–8 drive pool a third parity drive costs most of the
  remaining capacity. The role defaults to raidz2 for a fresh node; each
  region pins its real topology in inventory (`zfs_data_pool_type`). r1 is
  raidz1, see [storage-considerations.md](../storage-considerations.md#r1-capacity).
- **Size for the lake, not the archive.** The ClickHouse lake is the
  dominant store (14.6 TiB on r1, 2026-09-28); the raw galexie archive can
  be trimmed behind the public cold tier (ADR-0027). Current per-dataset
  figures: [storage-considerations.md](../storage-considerations.md#datasets).

### 3.4 Galexie backfill time

Stock `scan-and-fill` is single-goroutine on the S3 PUT side: ~59
ledgers/s on r1 (2026-04-25), about **12 days** for genesis to tip. Eight
disjoint-range processes in parallel (each with its own captive-core
`storage_path` and `admin_port`) reach ~470 ledgers/s, about **1.5 days**
([galexie-backfill.md](../../operations/galexie-backfill.md#tuning--when-60-ledgerssec-isnt-enough)).
Galexie, not core catchup, is the long pole when budgeting a new node.

### 3.5 Network

- ≥ 1 Gbps sustained public bandwidth; initial catchup saturates it for
  hours. One static public IPv4 (SCP peers cannot traverse NAT); IPv6 on.
- Port 11625 (SCP peer) open to the world. Everything else — 11626 core
  admin, Postgres, ClickHouse, MinIO, exporters — is internal only; SSH is
  keys only.

---

## 4. Upgrades

stellar-core upgrades follow SDF's rhythm: one region at a time, 24 h
soak, then the next. Never skip a protocol upgrade; flag days are
coordinated on SDF's `#validators` channel.

## 5. Open before buying owned hardware

Only relevant if a region moves off rented bare metal:

1. **ARM64.** Ampere-class servers are cheaper per core; stellar-core on
   ARM is community-tested, not SDF-blessed.
2. **HSM choice** for a validator node: see
   [validator-rollout.md](validator-rollout.md#6-open-questions).
