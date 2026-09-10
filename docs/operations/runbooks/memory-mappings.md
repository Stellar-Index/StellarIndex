---
title: Runbook — per-process memory-mapping headroom
last_verified: 2026-09-10
status: ratified
severity: P1 | P3
---

# Runbook — `stellarindex_process_mappings_*`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_process_mappings_high` (P3, ticket) · `stellarindex_process_mappings_critical` (P1, page) · `stellarindex_process_mappings_exhaustion_projected` (P1, page) · `stellarindex_process_mappings_probe_degraded` (P3, ticket) |
| Severity | P3 → P1 escalation |
| Detected by | Prometheus rules in `deploy/monitoring/rules/memory-mappings.yml` + `configs/prometheus/rules.r1/memory-mappings.yml` (byte-identical copies; group `stellarindex.memory_mappings`). Fired-state coverage: `deploy/monitoring/rule-tests/memory-mappings_test.yml`. |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/memory_mappings.prom`, refreshed every 5 min by `memory-mappings.timer` → `/usr/local/bin/memory-mappings` (`configs/ansible/roles/archival-node/files/memory-mappings.sh`) |
| Steady-state | ~47,000–50,000 mappings for ClickHouse against `vm.max_map_count` = 1,048,576 — about **4.5 %** of the limit (measured on r1, 2026-09-10, immediately after the restart) |
| Customer impact | None while the ticket is firing. At exhaustion the process cannot `mmap` at all: ClickHouse crashes, and the jobs behind it fail (`holders-rollup.service` did on 2026-09-10) |
| Companions | [host-memory-high](host-memory-high.md), [systemd-unit-failed](systemd-unit-failed.md) |

## Why this exists

On **2026-09-10 13:53 CEST** ClickHouse on r1 exhausted the kernel's
per-process virtual-memory-mapping limit. Its own error text:

```
Allocator: Cannot malloc 63.33 MiB: , errno: 12, strerror: Cannot allocate memory
It looks like that the process is near the limit on number of virtual memory mappings.
Current number of mappings (/proc/self/maps): 1048578.
Limit on number of mappings (/proc/sys/vm/max_map_count): 1048576.
```

It went into a `std::bad_alloc` storm and crashed; systemd auto-restarted
it (restart counter 1) and `holders-rollup.service` failed as collateral —
its first failure ever, against 24 successful cycles in the preceding 48 h.

The limit was **already** raised far above the kernel's 65530 default and
was **already** codified — `/etc/sysctl.d/10-map-count.conf`,
`vm.max_map_count=1048576` (2^20). What did not exist was any signal at
all: no metric, no rule, no reference to `max_map_count` anywhere in the
tree. **A server crash was the first and only signal.**

Note what is NOT known: at ~4.5 % steady state, the excursion to
1,048,578 was roughly **20x**, and its driver has not been established.
Nothing here should be read as a diagnosis. The probe exists so the next
excursion is watched on the way up rather than reconstructed afterwards,
and the first job when one of these fires is to **capture evidence**.

`node_exporter` has no per-process mapping series — its procfs collector
is system-wide — which is why this is a textfile producer.

## Quick diagnosis (≤ 5 min)

```sh
# 0. What the probe currently sees.
ssh r1 'cat /var/lib/node_exporter/textfile_collector/memory_mappings.prom'
#    ..._procs 0            → the probe is watching NOTHING (see below)
#    ..._unreadable 1       → it matched but could not read the map
#    ..._ratio              → the number the alerts compare

# 1. Find the pid the probe reports: the HIGHEST-mapping process off the
#    clickhouse binary. Never `pgrep -f clickhouse` — that self-matches
#    the command line you are typing. r1 runs TWO: a watchdog parent
#    (~97 mappings) and the real server (~47,000).
ssh r1 'for x in /proc/[0-9]*; do e=$(readlink -f $x/exe 2>/dev/null); \
  case "$e" in */clickhouse) echo "$(wc -l < $x/maps) ${x##*/}";; esac; done | sort -rn'

# 2. The live count and the live limit.
ssh r1 'wc -l < /proc/<pid>/maps; cat /proc/sys/vm/max_map_count'

# 3. WHAT the mappings are — the one measurement that narrows the cause.
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | sort | uniq -c | sort -rn | head -20'
#    Mostly file paths under /var/lib/clickhouse → file-backed growth
#      (data parts / marks being mmapped; look at part counts next).
#    Mostly blank (anonymous) → allocator / arena growth.
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | grep -c ^$'   # anonymous count

# 4. If file-backed, the obvious ClickHouse-side quantities to read.
#    These are candidates to MEASURE, not causes to assume.
ssh r1 'clickhouse-client -q "SELECT count() AS parts, sum(rows) FROM system.parts WHERE active"'
ssh r1 'clickhouse-client -q "SELECT * FROM system.asynchronous_metrics WHERE metric LIKE \"%Mmap%\" OR metric LIKE \"%OpenFile%\""'
ssh r1 'clickhouse-client -q "SELECT name, value FROM system.settings WHERE name IN (\"mmap_cache_size\",\"min_bytes_to_use_mmap_io\",\"local_filesystem_read_method\")"'
```

## Mitigation (≤ 15 min)

**Capture before you fix.** A restart clears the mappings and the
explanation with them, and the alert resolves — which is exactly how this
condition stayed invisible until it crashed a server.

```sh
ssh r1 'cp /proc/<pid>/maps /var/tmp/maps-$(date +%s).txt'
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | sort | uniq -c | sort -rn > /var/tmp/maps-summary-$(date +%s).txt'
```

Then, in order:

1. **Buy headroom** (`_critical` / `_exhaustion_projected`). Raising the
   limit is the standard remedy and takes effect immediately; it costs a
   little kernel memory per mapping. It is a stopgap — the driver is not
   established — but a crashed ClickHouse is worse than a larger ceiling.

   ```sh
   ssh r1 'sysctl -w vm.max_map_count=2097152'
   # Persist it, or the next boot silently reverts to 1048576:
   ssh r1 'sed -i s/1048576/2097152/ /etc/sysctl.d/10-map-count.conf && sysctl --system'
   ```

   The metric is published as a **ratio**, so every threshold moves with
   the limit automatically — no rule edit is needed after a change.
   `stellarindex_process_memory_mappings_limit` records what the probe
   read, so the change is visible on the graph.

   **`/etc/sysctl.d/10-map-count.conf` is NOT ansible-managed** — it is
   an out-of-band file on r1, and `vm.max_map_count` appears nowhere in
   this repo (checked 2026-09-10). The role's sysctl surface is the
   `sysctl_tunings` map in
   `configs/ansible/roles/archival-node/defaults/main.yml`, applied to
   `/etc/sysctl.d/90-stellarindex.conf`; sysctl.d applies in lexical
   order and the later file wins, so a value added there would take
   precedence over the hand-written one. Adding it is the right long-term
   move and is deliberately NOT done as part of an incident — an
   emergency `sysctl -w` plus a note is; codify it afterwards, in a
   change that can be reviewed with `--check --diff`.

2. **Restart ClickHouse** only if it is already failing to allocate, and
   only after step 0's capture. `systemctl restart clickhouse-server`.
   Expect the mapping count to return to the ~47,000 band within minutes;
   if it climbs straight back, that is a strong signal and worth its own
   incident.

3. **Watch the rollups behind it.** `holders-rollup.service` failed as
   collateral on 2026-09-10. `systemctl --failed` and the
   `stellarindex_systemd_unit_failed` ticket cover the rest.

### `probe_degraded` — the probe itself

```sh
ssh r1 'systemctl status memory-mappings.timer memory-mappings.service'
ssh r1 'journalctl -u memory-mappings.service --since -1h'
ssh r1 'bash scripts/ci/memory-mappings-test.sh'   # from a checkout: pins the shipped bytes
```

| Reading | Meaning | Action |
| ------- | ------- | ------ |
| `..._procs 0` | Matched **nothing**. The process is stopped, or its binary moved/was renamed. | The most dangerous state: the mapping gauges are *withheld* rather than published as 0, so every threshold alert above is silent and looks healthy. Confirm ClickHouse is running; if the exe path changed, update `MEMORY_MAPPINGS_WATCH` in `memory-mappings.service.j2`. |
| `..._unreadable 1` | Matched, but `/proc/<pid>/maps` or `vm.max_map_count` could not be read. | The unit must run as `User=root` — `/proc/<pid>/{exe,maps}` are readable only by the owning uid or root, and ClickHouse runs as `clickhouse`. Check the unit was not de-privileged and that `ptrace_scope` was not tightened. |
| unit `failed`, previous `.prom` intact | The probe rendered a line that does not parse as Prometheus exposition and **refused to publish**. | Read the journal — it names the offending line. node_exporter rejects an unparseable `.prom` *whole*, so refusing is deliberate: the stale file ages into this same ticket instead of taking every family in the file off the host (r1 2026-09-10, the timescale case). |
| stamp older than 30 min | The timer stopped firing. | node_exporter re-serves a stale textfile verbatim on every scrape, so the gauges *freeze at their last healthy value* rather than going absent — `..._updated_unix` is the only series that can see this. `systemctl start memory-mappings.timer`. |

## Known false-positive patterns

- **A ClickHouse restart** drops the count to near zero and climbs back
  to the steady band over a few minutes. The trend alert's `for: 5m` plus
  its `ratio > 0.10` floor keep that recovery from paging.
- **A deliberate `vm.max_map_count` change** moves the ratio for every
  process at once. That is the metric working: the thresholds are
  fractions of whatever limit the kernel currently reports.

## Related

- [host-memory-high](host-memory-high.md) — RSS pressure, a different
  resource from mapping count: many small mappings are cheap in resident
  bytes and expensive in map count, so these two can move independently.
- [systemd-unit-failed](systemd-unit-failed.md) — the catch-all that
  picks up `memory-mappings.service` when it refuses to publish.
- [zfs-pool-full](zfs-pool-full.md) — the other substrate limit that
  takes ClickHouse down.
- `docs/reference/metrics/README.md` — the metric family's reference entry.

## Changelog

| Date | Change |
| ---- | ------ |
| 2026-09-10 | Created after the ClickHouse `vm.max_map_count` exhaustion crash on r1. Probe, four alerts and this runbook landed together; the driver of the 20x excursion remains unestablished. |
