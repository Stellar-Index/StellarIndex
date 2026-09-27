---
title: Runbook — md-array-degraded
last_verified: 2026-09-27
status: draft
severity: P1
---

# Runbook — `stellarindex_md_array_degraded`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_md_array_degraded` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (group `stellarindex.infra`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/infra.yml`. |
| Typical MTTR | Hours — days (replacement + resilver depends on disk availability) |
| Impact | r1's OS/boot disks are mdadm RAID1 (installimage `SWRAID 1`): `md0` (swap), `md1` (`/`). A RAID1 array tolerates exactly one failed member — this alert fires at the moment that redundancy is lost, not yet a data-loss event. Reads and writes still serve on the surviving member; a second failure before replacement means the array (and, for `md1`, the whole host) is gone. |

## Symptoms

- `node_md_disks{state="failed"} > 0` for a `device` (`md0`/`md1`), for
  ≥ 5 min.
- `cat /proc/mdstat` shows `[_U]` or `[U_]` (one member marked `(F)` or
  missing) instead of `[UU]`.
- Kernel log: `md/raid1:mdX: Disk failure on <dev>, disabling device.`

## Quick diagnosis (≤ 5 min)

```sh
ssh <host> 'cat /proc/mdstat'
ssh <host> 'mdadm --detail /dev/md0 /dev/md1'
ssh <host> 'dmesg -T | grep -iE "md/raid1|ata.*error|nvme.*error" | tail -30'
```

## Mitigation

- [ ] Step 1 — identify the failed member from `mdadm --detail`.
- [ ] Step 2 — if the underlying drive is failing outright (not a
      transient link error), schedule replacement — see
      `nvme-smart.md` if the same device is also throwing NVMe SMART
      warnings, which is common (a failing drive often trips both).
- [ ] Step 3 — after physical replacement:
      `mdadm --manage /dev/mdX --add /dev/<new-partition>` and confirm
      the resync starts (`cat /proc/mdstat` shows a `resync` line with
      a percentage).
- [ ] Verification: `mdadm --detail /dev/mdX` reports `State : clean`
      and both members `active sync`; the alert clears once
      `node_md_disks{state="failed"}` returns to 0.

## Known false-positive patterns

- **A drive dropped transiently during a hot-plug event** (rare on
  this hardware) can re-add itself; confirm with `mdadm --detail`
  before treating it as a hard failure — but do not wait out the page
  on this alone.

## Related

- [`nvme-smart.md`](nvme-smart.md) — the underlying drive's own SMART
  health; a failing NVMe often trips both alerts together.
- [`zfs-degraded.md`](zfs-degraded.md) — the same redundancy-loss
  class on the `data` ZFS pool instead of the OS disks.
- `configs/libvirt/installimage-host.conf`,
  `docs/operations/r1-deployment-state.md` — the RAID1 layout this
  alert protects.

## Changelog

- 2026-09-27 — initial draft. `stellarindex_md_array_degraded` had no
  coverage at all before this — a failed OS-disk RAID member could go
  unnoticed until the second member failed.
