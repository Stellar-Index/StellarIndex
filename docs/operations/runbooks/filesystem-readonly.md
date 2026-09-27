---
title: Runbook — filesystem-readonly
last_verified: 2026-09-27
status: draft
severity: P1
---

# Runbook — `stellarindex_filesystem_readonly`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_filesystem_readonly` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (group `stellarindex.infra`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/infra.yml`. |
| Typical MTTR | 15 min (remount + fsck if clean) — hours (fsck on real corruption) |
| Impact | The kernel has remounted a filesystem read-only, almost always on an unrecoverable block I/O error. Every write to it now fails outright, not degraded: Postgres WAL if it's the DB volume (`stellarindex_timescale_primary_down` follows shortly), application logs and `/tmp` if it's root. Distinct from `stellarindex_node_root_disk_full` / `stellarindex_timescale_disk_full` — the filesystem still reports free space, it just refuses writes. |

## Symptoms

- `node_filesystem_readonly{fstype!~"tmpfs|squashfs|overlay|nsfs|ramfs"} == 1`
  for ≥ 5 min on some `mountpoint`.
- Kernel log: `EXT4-fs error ... remounting filesystem read-only` (or
  the XFS/ZFS equivalent).
- Application-level symptom depends on the mount: Postgres write
  errors and eventual crash if it's the DB volume; every process
  writing logs fails if it's root.

## Quick diagnosis (≤ 5 min)

```sh
ssh <host> 'mount | grep " ro,"'
ssh <host> 'dmesg -T | grep -iE "remount-ro|EXT4-fs error|XFS.*Corruption|I/O error" | tail -30'
ssh <host> 'smartctl -a /dev/<underlying-device>'   # is the disk itself failing?
```

## Typical root causes

1. **Unrecoverable disk I/O error** — the kernel protects against
   further corruption by forcing read-only. Check SMART / `dmesg` for
   the underlying drive; this often pairs with `nvme-smart.md` or
   `md-array-degraded.md` firing on the same device.
2. **Detected filesystem corruption** (metadata inconsistency) rather
   than a hardware fault — `fsck` will be required before remounting
   read-write.
3. **ZFS pool fault** on a dataset mounted through ZFS — see
   `zfs-degraded.md`; ZFS's own fault handling can also present as a
   read-only dataset.

## Mitigation

- [ ] Step 1 — confirm the underlying device is not actively failing
      (SMART / `dmesg` above). If it is, treat as a hardware
      replacement first — remounting rw onto a still-failing disk
      reproduces the same event.
- [ ] Step 2 — if the disk is sound: `fsck` the filesystem (offline,
      unmounted) to clear the inconsistency that triggered the
      remount, then remount read-write.
- [ ] Step 3 — if this is the DB volume: confirm Postgres survived or
      needs a restart/crash-recovery pass once the filesystem is
      read-write again (see `db-disk-full.md`'s WAL considerations —
      same volume, different trigger).
- [ ] Verification: `mount` no longer shows `ro` for the affected
      mountpoint; a test write succeeds; the alert clears within 5 min
      of `node_filesystem_readonly` returning to 0.

## Known false-positive patterns

- **A legitimately read-only mount not covered by the fstype
  exclusion** (e.g. a CD-ROM image, a deliberately mounted `ro` backup
  snapshot) — if this recurs, add the specific `mountpoint` to the
  exclusion rather than treating every occurrence as an incident.

## Related

- [`db-disk-full.md`](db-disk-full.md) — a different DB-volume failure
  mode (out of space, not read-only).
- [`nvme-smart.md`](nvme-smart.md), [`md-array-degraded.md`](md-array-degraded.md) —
  common underlying hardware causes.
- [`zfs-degraded.md`](zfs-degraded.md) — the ZFS-specific presentation
  of this failure class.

## Changelog

- 2026-09-27 — initial draft. `stellarindex_filesystem_readonly` had
  no coverage before this — a filesystem forced read-only by the
  kernel was invisible to every existing disk-full alert, since free
  space is unaffected.
