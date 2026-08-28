---
title: Runbook — galexie-archive tip-lag
last_verified: 2026-08-28
status: ratified
severity: P1 | P3
---

# Runbook — `stellarindex_galexie_archive_tip_lag_*`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_galexie_archive_tip_lag_high` (P3, warn) · `stellarindex_galexie_archive_tip_lag_severe` (P1, page) · `stellarindex_galexie_archive_tip_lag_metric_stale` (P3). Rule labels are `severity: ticket` (P3) / `severity: page` (P1), routed by `configs/alertmanager/alertmanager.r1.yml`. |
| Severity | P3 → P1 escalation |
| Scope | r1 / pubnet only — `galexie-archive-fill` is installed only when `stellar_network == pubnet` (aws-public has no testnet/futurenet dataset) and is actively removed elsewhere; the tip-lag updater IS installed on every network, so the remediation below does not apply off pubnet. |
| Detected by | Prometheus rules in `deploy/monitoring/rules/galexie-archive.yml` + `configs/prometheus/rules.r1/galexie-archive.yml` (byte-identical copies; group `stellarindex.galexie_archive_tip_lag`) |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom`, refreshed every 5 min by `galexie-archive-tip-lag.timer` → `/usr/local/bin/galexie-archive-tip-lag` |
| Steady-state | 0 → 64,000 ledgers, sawtooth: the fill mirrors only COMPLETE 64,000-ledger partitions (`files_per_partition=64000`, `ledgers_per_file=1`), so lag climbs for ~4 days per partition and drops to ~0 on the next hourly fill after a partition completes. A lag > 64,000 sustained means a completed partition was not mirrored. |
| Customer impact | None while alerting — live ingest unaffected; durable upstream `aws-public-blockchain` backstops the data. The R1 full-mirror integrity-leader role degrades. |
| Companions | [archive-files-missing](archive-files-missing.md), [bootstrap-archival-node](bootstrap-archival-node.md), [galexie-archive-contiguity](galexie-archive-contiguity.md) |

## Why this exists

The original `#26` incident was a **23-day silent stall** of
`galexie-archive` (the ADR-0016 R1 durable full-mirror). The
`galexie-archive-fill` catch-up script existed but only ran
manually — when its manual invocation drifted into oblivion,
nothing surfaced for weeks. The post-`#26` standing fix is the
hourly `galexie-archive-fill.timer`. **This alert is the safety
net under that:** if the timer itself (or its `mc` aliases, the
aws-public IAM, or a MinIO mtime-poison deadlock per the "mc mirror
gotcha") breaks silently, the lag will start growing and Prometheus
pages within hours — instead of the 23-day blindness `#26` was.

The thresholds were raised on 2026-05-22 (5,000 / 50,000 →
64,000 / 128,000) to match the partition model above: the old
values false-fired through every normal partition cycle.

## Quick diagnosis

```sh
# 1. Is the catch-up timer actively running on its hourly cadence (:17 + jitter)?
ssh r1 'systemctl list-timers galexie-archive-fill.timer galexie-archive-tip-lag.timer'
ssh r1 'systemctl status galexie-archive-fill.service'           # last run, exit code
ssh r1 'systemctl show galexie-archive-fill.service -p Result'

# 2. Did the most recent fill find no missing partitions (the
#    expected steady state)?
ssh r1 'tail -30 /var/log/galexie-mirror.log'                    # "needs work (total): 0" = healthy; also read the
                                                                  # "missing entirely:", "incomplete (queued by Phase 1b):"
                                                                  # and "below hot floor (...)" lines

# 3. Is the textfile metric itself stale? (metric_stale alert variant)
ssh r1 'ls -la /var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom'
ssh r1 'cat /var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom'
ssh r1 'sudo /usr/local/bin/galexie-archive-tip-lag --self-test'  # pins the object-name parser (single-ledger and
                                                                  # range forms, .zst/.zstd); no mc alias needed

# 4. Manually compute the gap (in case the script itself misreports).
#    Partition names are reverse-hex prefixed, so the first row is the newest:
ssh r1 'mc ls local/galexie-live | head -1; mc ls local/galexie-archive | head -1'
```

## Triage tree

- **`metric_stale` only** (lag value missing/old): the updater
  script broke. Read `journalctl -u galexie-archive-tip-lag.service -n 50` — usually mc-alias misconfig, permissions, or MinIO down. The metric file is the canary; restore it first so the other alerts can fire honestly.
- **`tip_lag_high` (>64,000 for 90m)**: one completed partition
  has stayed un-mirrored across two hourly fill cycles — the fill
  timer is slow or partially failing. Check
  `systemctl status galexie-archive-fill.service` + the last run in
  `/var/log/galexie-mirror.log`, `mc admin info local` for MinIO
  load, and aws-public listing latency. Do NOT wait further cycles;
  this is already past the tolerance window.
- **`tip_lag_severe` (>128,000 for 30m ≈ ≥2 completed partitions,
  ~8 days)**: the fill timer has clearly broken — same failure
  class as #26. Run
  `sudo systemctl start galexie-archive-fill.service`, then
  `journalctl -u galexie-archive-fill.service -n 100` and tail
  `/var/log/galexie-mirror.log` to see the live error; then
  root-cause the systemd timer/service. Page severity: R1's
  full-mirror integrity guarantee is broken; backfills / WASM-walks
  past the gap will fail for ledgers above the hot floor (below it
  the ADR-0027 cold tier serves reads).

## Remediation

```sh
# Manual catch-up (idempotent, the same unit the timer runs — via
# run-heavy-job.sh: singleton flock, MemoryMax=20G, disk watchdog,
# 6h TimeoutStartSec):
ssh r1 'sudo systemctl start galexie-archive-fill.service'

# Foreground run with the same guards (never invoke the script bare —
# it races the :17 timer on the shared /tmp/galexie-fill.*.txt scratch
# files and runs uncapped):
ssh r1 'sudo /usr/local/sbin/run-heavy-job.sh galexie-archive-fill /usr/local/bin/galexie-archive-fill'

# Force the tip-lag updater to re-read after manual remediation:
ssh r1 'sudo systemctl start galexie-archive-tip-lag.service'

# Re-enable the recurring timer if it was disabled:
ssh r1 'sudo systemctl enable --now galexie-archive-fill.timer galexie-archive-tip-lag.timer'
```

If `galexie-archive-fill` itself fails, follow its log to
`/var/log/galexie-mirror.log` — common causes: bucket permission
denied (rotate the `mc alias` creds; note the `aws-public` alias
lives only in root's `~/.mc/config.json` and is not ansible-managed),
aws-public listing 503s (retry in an hour), MinIO local out of space
(check `zpool list -o name,size,alloc,free,cap data`). Partitions
ending below `ARCHIVE_HOT_FLOOR` in `/etc/default/galexie-archive-fill`
(49,984,000 on r1 since the 2026-07-26 trim) are intentionally not
mirrored — the ADR-0027 cold tier serves them and
`galexie-archive-trim.timer` removes them — so a "missing" partition
below the floor is not a fill failure.

## Related

- ADR-0016 — per-region storage strategies (defines R1 = full mirror).
- ADR-0027 — LCM cache tiering (hot floor + trim; `docs/operations/lcm-cache-tiering.md`).
- `#26` — the originating 23-day silent-stall incident.
- `#7` — LCM-cache tiering (longer-term capacity strategy).
- [archive-files-missing](archive-files-missing.md) — sibling Tier-A/B archive integrity alert.
- [galexie-archive-contiguity](galexie-archive-contiguity.md) — the middle-is-intact guard sharing this rule file (`stellarindex_galexie_archive_gap`, `_contiguity_silent`).
- `galexie-archive-fill.{service,timer}` — canonical source `configs/ansible/roles/archival-node/templates/systemd/*.j2`, installed by `tasks/07-galexie.yml`; the `deploy/systemd/` copies have drifted (bare `ExecStart`, no `run-heavy-job.sh` wrapper) and are not what runs on r1.
