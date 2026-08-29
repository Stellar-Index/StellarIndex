---
title: Runbook — zfs-degraded
last_verified: 2026-08-29
status: current
severity: P1
---

# Runbook — `stellarindex_zfs_pool_degraded`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_zfs_pool_degraded` |
| Severity | P1 (page — SEV-1) |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (group `stellarindex.infra`, `severity: page`, `for: 60s`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/infra.yml`. |
| Typical MTTR | 30 min – hours (resilver time depends on data size) |
| Impact | The `data` pool is **raidz1 — SINGLE parity**. It tolerates exactly ONE drive failure, so at DEGRADED there is **ZERO remaining redundancy**: one FAULTED drive IS the edge, and any second failure (or an unrecoverable read error during resilver) means data loss. Reads and writes still serve while DEGRADED — do not mistake that for margin. |

> **Settled 2026-08-29 (#289): raidz1.** The ansible *default* is
> raidz2, but that default describes a fresh archival node, never r1 —
> r1's inventory pins `zfs_data_pool_type: "raidz1"`, matching the
> 2026-07-17 live review ("ZFS raidz1 (single parity, NOT raidz2)",
> `docs/audit/audit-2026-07-16/go-live-master-plan.md` §5) and commit
> `ca2f4748`. Capacity settles it without host access too: the ~16.8 TB
> live footprint measured that day does not fit the ~13.85 TB a
> second parity drive would leave. `scripts/ci/lint-docs.sh` §18 now
> lints every r1-scoped file against the inventory, so this cannot
> drift back. This runbook must never promise two-failure tolerance.

## Symptoms

- `node_zfs_zpool_state{state=~"degraded|faulted|unavail|suspended"} > 0`
  for ≥ 60 s (lowercase states, pool in the `zpool` label — the
  F-1329 repoint from the never-emitted `node_zfs_pool_state`).
- `zpool status` shows one (or more) drives in FAULTED or OFFLINE.
- Often pairs with `nvme-smart.md` firing on the same host — the
  IO errors escalated to a full drive fail.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96 'zpool status -v data'
# Look for lines like:
#   state: DEGRADED
#   action: Attach the missing device and online it...
#   NAME                STATE     READ WRITE CKSUM
#   data                DEGRADED     0     0     0
#     raidz1-0          DEGRADED     0     0     0
#       nvme0n1         ONLINE       0     0     0
#       nvme1n1         FAULTED      0     0     0  too many errors

# Which drive physically?
ssh root@136.243.90.96 'ls -l /dev/disk/by-id/ | grep nvme1n1'
ssh root@136.243.90.96 'nvme list'   # serial → slot mapping for the Hetzner ticket

# Resilver status if replacement already in progress
ssh root@136.243.90.96 'zpool status data | grep -A5 resilver'
```

## Typical root causes

1. **Drive hardware failure.** The usual story — wear, thermal,
   controller. `nvme-smart.md` will have been firing on this drive
   for a while if you're paying attention.

2. **SATA/NVMe controller flake** — drive is fine but the slot /
   cable / controller is dropping it.

3. **Power event** — brownout, PSU swap mid-operation.

## Mitigation

- [ ] Step 1 — **escalate to SEV-1 immediately.** raidz1 tolerates
      ONE failure; at DEGRADED that tolerance is already spent. You
      are one failure — or one unrecoverable read error during the
      resilver — from data loss. There is no "one margin left"
      state on this pool. Treat everything from here as a race
      against the resilver clock.
- [ ] Step 2 — **quiesce heavy writers** before and during the
      resilver: pause pgBackRest (`systemctl stop
      pgbackrest-backup.timer` for the window), no scrubs, no heavy
      one-shot jobs (`run-heavy-job.sh` queue stays empty). Every IO
      cycle spent elsewhere extends the window of zero redundancy.
- [ ] Step 3 — physically replace the drive. Open a Hetzner support
      ticket with the drive serial (from `nvme list`) for the swap.
- [ ] Step 4 — tell ZFS to resilver:
      ```sh
      zpool replace data <old-drive-id> <new-drive-id>
      ```
- [ ] Step 5 — monitor resilver progress. ETA in `zpool status`.
      Keep the pool quiesced until it completes.
- [ ] Step 6 — if a second drive fails during resilver, the pool is
      lost. The DR path is NOT a failover (there is no replica): it
      is the disaster-recovery triage tree in
      `docs/operations/archival-node-bringup.md` — rebuild the box,
      restore Postgres from pgBackRest, re-mirror galexie data from
      the SDF / `aws-public-blockchain` mirrors per
      [ADR-0016](../../adr/0016-per-region-storage-strategy.md).
- [ ] Verification: `zpool status data` shows ONLINE, no errors, pool
      returns to HEALTHY;
      `node_zfs_zpool_state{zpool="data",state="online"} == 1`.

## Root cause analysis

- Smartctl logs from the failed drive (before it's discarded —
  the vendor's warranty process may need them).
- When was the drive installed? (Check
  `docs/operations/r1-deployment-state.md` and the Hetzner order
  records — there is no separate hardware-inventory doc.)
- Did `nvme-smart.md` or `nvme-thermal.md` fire earlier? Was
  action taken?
- Are other drives on the same host showing elevated SMART
  warnings?

## Known false-positive patterns

- **During a planned drive replacement** the alert fires
  momentarily as the pool transitions DEGRADED → resilvering →
  HEALTHY. Silence during scheduled maintenance windows.

## Related

- `nvme-smart.md` — precursor warnings.
- `nvme-thermal.md` — another precursor (alert currently inert; see
  that runbook's banner).
- `db-disk-full.md` — running tight on capacity amplifies
  recovery stress.
- `zfs-pool-full.md` — the capacity side of the same pool
  (near-full → ZFS copy-on-write write stalls). Parity here;
  capacity there.
- `docs/operations/archival-node-bringup.md` — the
  disaster-recovery triage tree if the pool is lost.
- Tier-1 posture: `docs/adr/0004-tier1-validator-aspiration.md` —
  we're committed to independent history archives, which means
  drive failures on one of the three validator hosts must not
  cascade to the others.

## Changelog

- 2026-08-29 — the raidz1-vs-raidz2 TODO is **settled: raidz1** (#289).
  r1's inventory now pins `zfs_data_pool_type: "raidz1"` and is the
  linted authority (`scripts/ci/lint-docs.sh` §18); the role's raidz2
  default is documented as fresh-node-only. Nothing in the procedure
  below changes — it was already written for single parity.
- 2026-08-28 — re-verified against HEAD. Pool is raidz1 (single
  parity), not raidz2: at DEGRADED there is zero remaining
  redundancy, so the "verify remaining margin" step became escalate
  immediately + quiesce heavy writers + treat resilver as a race
  (the raidz1-vs-raidz2-default question that left open was settled
  on 2026-08-29, above). Metric expr replaced with the real
  `node_zfs_zpool_state{state=~"degraded|..."}` (F-1329 repoint;
  lowercase states, `zpool` label, `for: 60s`). Pool name `tank` →
  `data`, vdev `raidz2-0` → `raidz1-0`; "failover-to-replica" → the
  real DR path (`archival-node-bringup.md` triage + pgBackRest +
  ADR-0016 mirrors); dead `docs/operations/inventory.md` pointer →
  `r1-deployment-state.md` / Hetzner order records; hot-spare
  false-positive removed (r1 has no spare drive). Rule citation →
  `rules.r1/infra.yml`; commands rehosted to `ssh root@136.243.90.96`.
- 2026-04-23 — initial draft.
