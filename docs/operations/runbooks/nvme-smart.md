---
title: Runbook — nvme-smart
last_verified: 2026-07-25
status: draft
severity: P2
---

# Runbook — NVMe drive health

Covers four alerts, all of which end in the same decision — *do we replace a
drive, and how soon* — but which fire at very different points on that curve:

| Alert | What it means | Urgency |
| ----- | ------------- | ------- |
| `stellarindex_nvme_smart_warn` | An IO error reached the kernel | P2 |
| `stellarindex_nvme_wear_high` | >80% of rated write endurance consumed | P3 — procurement trigger |
| `stellarindex_nvme_spare_low` | <20% reserve blocks left | **P1** — late-stage, moves shortly before failure |
| `stellarindex_nvme_media_errors` | New uncorrected media errors in 24h | P2 |

Read `wear_high` and `spare_low` together. `percentage_used` is a *predicted*
endurance figure and can sit above 80% for a long time without trouble;
`available_spare` falling is the drive actually running out of blocks to
remap, which is a much later and more serious signal. Wear high + spare full
is a planning problem. Spare low is an incident.

This matters more than the parity margin suggests — the `data` pool
is raidz1 (single parity; see `zfs-degraded.md`), so one failing
drive already puts the pool at zero remaining redundancy. r1's NVMe is not
replaceable on demand, so the value of these alerts is lead time. Treat
`wear_high` as the moment to start a procurement conversation, not the moment
to watch it.

> **Note (2026-07-25):** until this date the packaged nvme collector wrote to
> `/var/lib/prometheus/node-exporter` while node_exporter read
> `/var/lib/node_exporter/textfile_collector`, so `nvme_percentage_used_ratio`,
> `nvme_available_spare_ratio` and `nvme_media_errors_total` were generated
> every 5 minutes and discarded — none of the three wear alerts could fire.
> Fixed with a systemd drop-in in the archival-node role. If these series are
> missing again, check that redirect first.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_nvme_smart_warn` |
| Severity | P2 (ticket — schedule replacement, don't panic) |
| Detected by | `deploy/monitoring/rules/infra.yml` |
| Typical MTTR | hours – days (replacement lead time) |
| Impact | Not immediately customer-visible. An IO error is the drive saying "I'm starting to fail." The raidz1 `data` pool tolerates exactly ONE full drive failure; a single IO error is fine — until it isn't, and there is no second margin. |

## Symptoms

- `increase(node_disk_io_errors_total[1h]) > 0` on some device.
- Kernel dmesg shows `blk_update_request: I/O error` or the NVMe
  equivalent.
- The drive's SMART attributes may not yet flag a failure.

## Quick diagnosis (≤ 5 min)

```sh
ssh <host> 'dmesg -T | grep -iE "i/o error|nvme|media error" | tail'

# Full SMART dump — look at Media/Data Integrity Errors,
# Power On Hours, Percentage Used, Critical Warning flags.
ssh <host> 'smartctl -a /dev/nvme0n1'

# ZFS's view — any checksum errors on this drive's zpool?
ssh <host> 'zpool status -v'
```

## Typical root causes

1. **Drive wear at end-of-life.** `Percentage Used > 80%` on
   enterprise drives means it's nearing its DWPD ceiling. This is
   expected; replace on schedule.

2. **Controller flaking.** One NVMe drive producing intermittent
   errors while its siblings on the same host are fine — usually
   the drive, occasionally the backplane / cable / slot.

3. **Firmware bug.** Rare but real — some NVMe firmwares have
   specific failure modes under sustained load. Check the
   vendor's advisory page.

4. **Spurious kernel event.** Some kernel versions report IO
   errors that don't actually correspond to drive-level failures.
   If SMART is clean and ZFS checksum errors are zero, this may
   be the kernel's fault — upgrade the kernel when feasible but
   de-prioritise.

## Mitigation

- [ ] Step 1 — read SMART attributes + ZFS status (above). Judge
      severity.
- [ ] Step 2 — if ZFS already flagged checksum errors and is
      scrubbing/resilvering: this is escalating; follow
      `zfs-degraded.md`.
- [ ] Step 3 — schedule a drive replacement. Bring the replacement
      to the colo, offline the suspect drive (`zpool offline`),
      swap, `zpool replace`, wait for resilver.
- [ ] Step 4 — if the host is safe to drain: drain it out of any
      HAProxy pool fronting it (`disable server <pool>/<host>` on
      each LB), stop the relevant `stellarindex-*` units, then
      reboot after swapping. ZFS resilver is online-safe but
      stressed drives sometimes fail harder during resilver, so a
      clean reboot before the swap is preferable when traffic can
      be drained.
- [ ] Verification: new drive resilvered, `zpool status` returns
      to ONLINE, no new IO errors for 24 h.

## Known false-positive patterns

- **Single transient IO error at boot** — drives occasionally
  report spurious errors during initialization. If it doesn't
  recur within an hour, close the ticket but keep the drive on
  a watch list.
- **ZFS scrub detecting then repairing** — during scrub, ZFS reads
  everything; if a sector is marginal it gets flagged + repaired
  from parity. Count of "scrub errors repaired" is informational,
  not alarming unless growing.

## Related

- `zfs-degraded.md` — next step if IO errors escalate into pool
  degradation.
- `nvme-thermal.md` — thermal throttling is a precursor to wear
  issues on poorly-cooled drives.

## Changelog

- 2026-04-23 — initial draft.
