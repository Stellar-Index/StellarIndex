---
title: Runbook — host-memory-high
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_host_memory_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_host_memory_high` |
| Severity | P3 (informational, can escalate quickly) |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (group `stellarindex.infra`; `severity: informational`, `for: 10m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/infra.yml`. |
| Typical MTTR | 15 min – hours |
| Impact | Allocation-failure risk. The ansible role sets `vm.overcommit_memory=2` / `vm.overcommit_ratio=80` (strict overcommit), so the typical failure on r1 is **malloc/fork returning ENOMEM ("Cannot allocate memory") in whichever process allocates next** — predictable and log-visible — *before* the kernel OOM-killer ever runs. Heavy one-shot jobs are separately fenced by `run-heavy-job.sh` (`MemoryMax=20G`, `MemorySwapMax=0`) and galexie carries `MemoryLow=16G`, so a ballooning batch job is killed at its own cgroup wall rather than starving the consensus-critical services (the 2026-07-05 galexie wedge is the canonical case that motivated the fences). |

## Symptoms

- `(MemTotal - MemAvailable) / MemTotal > 0.90` for ≥ 10 min.
- Swap traffic. r1 HAS swap — 16 G at `vm.swappiness=1` — so with
  swappiness that low, **any sustained swap in/out
  (`vmstat 1` si/so columns) is itself a red flag**, not routine
  behaviour.
- Services logging "Cannot allocate memory" (ENOMEM) — under
  strict overcommit this is the failure you see INSTEAD of an
  OOM-kill.
- Subsequent alerts: `host-cpu-high` (swapping is CPU-visible),
  eventually a service alert when allocations start failing.

## Quick diagnosis (≤ 5 min)

```sh
# Top memory users (per-process and per-cgroup)
ssh root@136.243.90.96 'ps auxww --sort=-%mem | head -10'
ssh root@136.243.90.96 'systemd-cgtop --order=memory --iterations=2 -n 20'

# What's the breakdown? Page cache vs RSS vs slab?
ssh root@136.243.90.96 'free -h; cat /proc/meminfo | head -30'

# Strict overcommit means the primary evidence is ENOMEM in the
# journal, not an OOM-kill in dmesg — check both:
ssh root@136.243.90.96 'journalctl --since -2h | grep -i "cannot allocate memory" | tail'
ssh root@136.243.90.96 'dmesg -T | grep -i "out of memory\|killed process" | tail'

# Is a fenced heavy job the consumer (or was one just killed at
# its 20G MemoryMax wall)?
ssh root@136.243.90.96 'systemctl status "run-heavy-*.scope" --no-pager'
```

## Typical root causes

1. **Postgres `shared_buffers` + `work_mem` adding up.** Per-
   backend `work_mem` × `max_connections` is the usual way
   Postgres memory explodes.
   - Signal: `ps` shows many postgres backends each using
     hundreds of MB.
   - Mitigation: lower `work_mem` and/or cap `max_connections` via
     the `archival-node` ansible role's postgres vars, re-apply,
     and restart `postgresql@15-main`. There is no PgBouncer and
     no replica in this deployment — a restart is a brief
     served-tier outage; coordinate per the SEV playbook.

2. **Go runtime heap growth** — a memory leak or retention bug.
   - Signal: the binary's RSS climbs monotonically over hours/days.
   - Mitigation: pprof heap dump, then restart the systemd unit
     (`systemctl restart stellarindex-<binary>`) while the fix
     ships. For the indexer this loses no data (it resumes from
     the persisted cursor); the API restart is a brief blip behind
     Caddy.

3. **File-cache eating "available" memory that's not really
   available**. Linux's `MemAvailable` is usually correct but
   specific workloads (big mmap, tmpfs with ulimit) can create
   divergence.
   - Signal: `free -h` shows large `buff/cache` and low `available`.
   - Mitigation: usually benign — page cache is reclaimable. But
     if `available` is low AND applications are getting ENOMEM,
     that's a real problem.

4. **ZFS ARC.** No ARC cap is codified — `zfs_arc_max` appears
   nowhere in the ansible role, so ARC follows the ZFS default
   (up to ~half of RAM, shrinking under pressure).
   `TODO(ash): if a hand-set ARC cap exists live on r1, codify it
   in the role (CLAUDE.md ansible-drift rule); if not, decide
   whether one is wanted.`
   - Signal: `arcstat` / `/proc/spl/kstat/zfs/arcstats` shows ARC
     size close to RAM size.
   - Mitigation: ARC is reclaimable and usually self-corrects;
     a genuine cap belongs in the role, not a one-off
     `echo > /sys/module/zfs/...` hand fix.

## Mitigation

- [ ] Step 1 — identify the consumer (above).
- [ ] Step 2 — if a service is leaking: restart it (buys time)
      and file an incident.
- [ ] Step 3 — if Postgres: lower `work_mem` / cap
      `max_connections` via the ansible role and restart
      `postgresql@15-main`. This is a brief served-tier outage
      (single primary, no replica, no PgBouncer) — coordinate per
      the SEV playbook.
- [ ] Step 4 — if genuine undersize: scale up the host or move
      workloads.
- [ ] Verification: `available` memory climbs back to > 20 %; no
      new "Cannot allocate memory" journal lines (and no OOM
      events) in the next hour.

## Known false-positive patterns

- **ZFS ARC looking like "used" memory.** `MemAvailable` in the
  Linux kernel accounts for reclaimable page cache but historically
  not ZFS ARC. On newer kernels (6.x) this is fixed; older kernels
  can report 90 %+ used when half of that is ARC and immediately
  reclaimable. Check `arcstat` before panicking.
- **Freshly started process warming up**. `free -h` shows low
  available for the first few minutes while caches populate;
  stabilises.

## Related

- `host-cpu-high.md` — swapping shows up as CPU too.
- `timescale-primary-down.md` — OOM-kill of Postgres is a specific
  path to this.
- `host-down.md` — if the host itself goes (OOM-killer gets init?).

## Changelog

- 2026-04-23 — initial draft.
- 2026-08-29 — re-verified against HEAD; the draft described a
  deployment that never existed. PgBouncer drain +
  "primary-replica swap" mitigations removed (neither exists —
  single Postgres primary): the real lever is lowering `work_mem`
  / capping `max_connections` via the ansible role and restarting
  `postgresql@15-main` (brief served-tier outage, coordinate per
  SEV playbook). "Restart the pod" → restart the systemd unit.
  The "we cap ARC at ~50 % of RAM" claim was uncodified fiction
  (`zfs_arc_max` appears nowhere in ansible) — replaced with the
  ZFS-default reality + TODO(ash) to codify if a live hand-set cap
  exists. Swap: r1 HAS 16 G swap at `vm.swappiness=1`, so
  sustained swap traffic is itself a red flag (the old "swap
  mostly isn't enabled" line was wrong). OOM framing rewritten for
  `vm.overcommit_memory=2` / `ratio=80`: the typical failure is
  malloc/fork ENOMEM ("Cannot allocate memory") BEFORE the
  OOM-killer; heavy jobs are fenced by run-heavy-job.sh
  (MemoryMax=20G / MemorySwapMax=0) and galexie carries
  MemoryLow=16G (2026-07-05 wedge = canonical case) — added to
  Impact + diagnosis (journalctl ENOMEM grep,
  `systemctl status run-heavy-*.scope`). Detected-by converted to
  the dual-tree convention; ssh commands use the r1 shape.
