---
title: Runbook — node-root-disk-filling-fast
last_verified: 2026-07-03
status: current
severity: P1
---

# Runbook — `stellarindex_node_root_disk_filling_fast`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_node_root_disk_filling_fast` |
| Severity | **P1** (page) |
| Detected by | `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml` |
| Typical MTTR | 5–20 min |
| Impact | Root (/) is trending to 0 bytes within 30 min on a 10-minute linear fit. If it reaches full: Redis MISCONF blocks every cache write (`/v1/price` 404s), Postgres can't write its log and crashes, journald corrupts. Incident class: 2026-05-10, 2026-05-13, 2026-06-11 (`internal/incidents/data/2026-06-11-clickhouse-log-channel-wedge-root-full.md`). |

## Why this alert exists

The static `node_root_disk_full` (<10%) page is correct but too slow
for a log-flood: the 2026-06-11 ClickHouse log-channel wedge filled
root at **~3.8 GB/min** — healthy to full in ~5 minutes. This alert
fires on the *trend*, buying the 20+ minutes the static one can't.

## Symptoms

- Root free-space graph is a straight line pointing at zero.
- `journalctl -f` shows one unit repeating at very high rate
  (2026-06-11 signature: `clickhouse-server` emitting
  `Cannot log message in OwnAsyncSplitChannel` / Poco rotate stacks).

## Diagnosis (60 seconds)

```sh
df -h /
du -xs /var/log/* /tmp /var/cache 2>/dev/null | sort -rh | head -8
journalctl --since "-5min" --no-pager | awk '{print $5}' | sort | uniq -c | sort -rn | head -5
```

The last command names the flooding unit directly.

## Remediation

1. **If the flooder is clickhouse-server** (the known wedge): freeing
   space does NOT unwedge the log channel — **restart CH**:
   `systemctl restart clickhouse-server`. Then free space (step 3).
2. **Any other flooder**: stop or restart the unit; its journald
   output is rate-limited but check `/var/log/syslog` growth — if the
   unit is not covered by `/etc/rsyslog.d/10-suppress-noisy-units.conf`,
   add a `stop` rule there (and to ansible role 15-log-discipline.yml).
3. **Free space fast**: `journalctl --vacuum-size=200M`;
   `rm /var/log/syslog.1` (already-rotated copy); truncate the live
   offender file if needed: `: > /var/log/<offender>`.
4. Verify Redis + Postgres recovered: `redis-cli ping`,
   `systemctl is-active postgresql` (see
   [redis-write-blocked-disk-full](redis-write-blocked-disk-full.md)).

## Prevention state (2026-07-03)

- CH logs live on ZFS (`config.d/zzz-logpath.xml`) — the primary
  2026-06-11 writer can't touch root.
- journald capped at 500M (`journald.conf.d/00-cap.conf`).
- rsyslog drops loki + clickhouse-server unit output from syslog
  (`10-suppress-noisy-units.conf` — applied 2026-07-03; forensics
  showed it was NEVER live on r1, only codified in ansible role
  15-log-discipline.yml, which does not auto-run against r1 — the
  2026-06-11 postmortem recorded codified-as-applied).
- Open margin item: 16G of the 49G root is a swap file
  (`/swap_f1209`, ~1G used, with a separate 4G md0 swap partition) —
  dropping it doubles root headroom. Operator decision.

## Related

- [node-root-disk-full](node-root-disk-full.md) — the static threshold page.
- [node-root-disk-warning](node-root-disk-warning.md) — the 20% ticket.
- [redis-write-blocked-disk-full](redis-write-blocked-disk-full.md) — the downstream cascade.
