---
title: Runbook — host-swap-activity
last_verified: 2026-09-03
status: current
severity: P2
---

# Runbook — `stellarindex_host_swap_activity`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_host_swap_activity` |
| Severity | P2 (ticket) |
| Detected by | `configs/prometheus/rules.r1/galexie-archive.yml` (group `stellarindex.galexie_archive`; `expr: rate(node_vmstat_pswpout[10m]) > 100`, `severity: ticket`, `for: 15m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/galexie-archive.yml`. |
| Typical MTTR | 15 min (stop the over-budget job) – hours (re-plan the workload) |
| Impact | Not customer-visible on its own. It is the early warning for the pressure class that wedged galexie's captive core for 11 h on 2026-07-05: a process that swaps mid-write can come back with invalid local state. |

## Symptoms

- `rate(node_vmstat_pswpout[10m]) > 100` sustained 15 min — pages
  written OUT to swap, i.e. the kernel is evicting anonymous memory,
  not just reclaiming page cache.
- r1 has 16 G of swap at `vm.swappiness=1`
  (`configs/ansible/roles/archival-node/defaults/main.yml`,
  `sysctl_tunings`). At that swappiness the kernel swaps only under
  genuine pressure, so **any** sustained pswpout is a budget being
  violated, not routine behaviour.
- Often arrives with, or just before, `host-memory-high` and
  `host-cpu-high` (swapping is CPU- and IO-visible).

## Why this is its own alert

Every heavy one-shot on r1 is required to run under
`/usr/local/sbin/run-heavy-job.sh`, which caps the job at
`MemoryMax=20G` **with `MemorySwapMax=0`** — a job at its ceiling is
killed, it does not swap. Galexie carries `MemoryLow=16G` so reclaim
walks past it. Both fences are codified in
`configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`.
Sustained swapping therefore means memory is being consumed by
something OUTSIDE those fences — a heavy job run raw, a service leak,
or a genuinely undersized host — which is a different question from
"is the host near its memory ceiling" (`host-memory-high`).

## Quick diagnosis (≤ 5 min)

```sh
# Is it still swapping, and in which direction?
ssh root@136.243.90.96 'vmstat 1 5; free -g'

# Which processes hold swap? (sorted, largest last)
ssh root@136.243.90.96 'for p in /proc/[0-9]*; do s=$(awk "/VmSwap/{print \$2}" $p/status 2>/dev/null); [ -n "$s" ] && [ "$s" -gt 0 ] && echo "$s KB $(cat $p/comm)"; done | sort -n | tail -20'

# Is an UNWRAPPED heavy job the consumer? A scoped one cannot swap
# (MemorySwapMax=0), so anything swapping is outside the fence.
ssh root@136.243.90.96 'systemctl status "run-heavy-*.scope" --no-pager; systemd-cgtop --order=memory --iterations=2 -n 20'
```

## Typical root causes

1. **A heavy ops job run raw** instead of under
   `run-heavy-job.sh` — the 2026-07-05 case. It competes at full
   weight and, with no `MemorySwapMax=0`, pushes the captive core's
   pages out.
   - Mitigation: stop it, re-run it under the wrapper.
2. **A service leaking heap.** RSS climbs monotonically over
   hours/days until reclaim starts evicting neighbours.
   - Mitigation: pprof heap dump, restart the unit, file an incident.
3. **Postgres `work_mem` × concurrent backends.** See
   [host-memory-high](host-memory-high.md) — the mitigation (lower
   `work_mem` / cap `max_connections` via the ansible role) is the
   same and is a brief served-tier outage.
4. **Genuine undersize.** Nothing is misbehaving and the working set
   no longer fits. This is a capacity decision, not an incident.

## Mitigation

- [ ] Step 1 — identify what holds swap (above).
- [ ] Step 2 — if an unwrapped heavy job: stop it and re-run under
      `/usr/local/sbin/run-heavy-job.sh`.
- [ ] Step 3 — check galexie survived the pressure before closing:
      the lake tip must still be advancing. If the journal shows
      `Skipping catchup: incompatible core version or invalid local
      state`, this has already escalated — go to
      [galexie-catchup-refused](galexie-catchup-refused.md).
- [ ] Step 4 — if a service is leaking: restart it to buy time and
      file an incident.
- [ ] Verification: `rate(node_vmstat_pswpout[10m])` back to 0 and
      `free -g` shows swap used flat (used swap does NOT fall on its
      own — pages stay out until touched; flat is the clear signal,
      not zero).

## Known false-positive patterns

- **Swap-in without swap-out.** A process touching pages that were
  evicted hours ago drives `pswpin`, not `pswpout`; the alert reads
  `pswpout` only and will not fire on it.
- **A one-off eviction burst under 15 min** — a backup or compression
  window briefly winning against the page cache. The `for: 15m`
  absorbs it.

## Related

- [galexie-catchup-refused](galexie-catchup-refused.md) — where this
  escalates to if the captive core takes swap mid-write; that runbook
  names swap activity as its own step-3 check.
- [host-memory-high](host-memory-high.md) — the near-the-ceiling
  signal; shares the per-process diagnosis and the Postgres
  mitigation.
- [host-cpu-high](host-cpu-high.md) — swapping shows up as CPU and
  iowait too.

## Changelog

- 2026-09-03 — created. The alert previously pointed at
  `galexie-catchup-refused.md`, whose "At a glance" (page severity,
  first move `systemctl restart galexie`) and "What fired" describe a
  wedged captive core — not a swapping host, which is the earlier and
  more common state.
