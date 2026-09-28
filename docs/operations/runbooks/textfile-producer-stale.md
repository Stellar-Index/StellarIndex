---
title: Runbook — textfile-producer-stale
last_verified: 2026-09-25
status: current
severity: P3
---

# Runbook — `stellarindex_textfile_producer_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_textfile_producer_stale` |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`; matches `node_textfile_mtime_seconds` with no `file=` selector |
| Typical MTTR | 5–30 min |
| Impact | Every family in the stale `.prom` file is frozen at its last-written value; node_exporter keeps re-serving it, so nothing that only reads those series would otherwise notice. |

## Why this exists

GH-899: 60 of 66 producers in `scripts/ci/textfile-producers.manifest`
had no staleness alert at all — a dead timer or crashed producer went
unalarmed forever. Rather than hand-writing 60 near-identical
per-file rules, this one has no `file=` selector, so it matches every
textfile-collector `.prom` node_exporter has ever scraped, present or
future. A producer with its own tighter, file-scoped alert
(`stellarindex_patroni_textfile_stale`,
`stellarindex_config_assertions_stale`, and the redis/keepalived/
data-freshness/stellar-stack-version alerts) fires sooner; this is
the backstop for everything else, at a deliberately loose 24h so it
never duplicates a tighter alert.

`scripts/ci/lint_textfile_exposition.py` enforces the other half:
every manifest producer's `.prom` output must be selected by a rule
(this one or a dedicated one), so a new producer with neither is a CI
failure, not a silent gap.

**2026-09-27: three exclusions added**, each a false-fire the 14d
Prometheus review found, none a dead producer:

- `ops_job_*.pid<N>.prom` — the fallback file `opsutil.pidPath` writes
  when a run loses the heartbeat flock. Nothing rewrites it again once
  that run exits (`sweepStalePIDFiles` only reaps it while another
  primary run is active), so it is an orphan by construction.
- `ops_job_backfill.prom` — backfill is a one-off maintenance job with
  no timer; a long gap between runs is expected, not a dead cron.
- `restore_drill*.prom` — monthly timer against this rule's 24h
  threshold. Now carries its own threshold instead:
  `stellarindex_restore_drill_textfile_stale` (35d, same file below).

**2026-09-28: `ops_job_usd_volume_restamp.prom` added** — same class as
`ops_job_backfill.prom` above: `usd-volume-restamp` is a manual one-off
`stellarindex-ops` job with no timer/cron, so a long gap between runs
is expected, not a dead cron.

## Diagnosis

```sh
# $labels.file is the full textfile-collector path, e.g.
# /var/lib/node_exporter/textfile_collector/ch_schema_snapshot.prom
grep -F "$(basename "$file")" scripts/ci/textfile-producers.manifest
systemctl status <the producer's timer/service>
journalctl -u <the producer's service> -n 50
```

Common causes: the producer's timer disabled by an ansible re-apply
that didn't restart it, the producer script erroring before its
atomic `mv` of the rendered file, or a dependency (patroni's REST
API, ClickHouse, S3/MinIO) it queries being unreachable.

## Related

- [patroni-textfile-stale](patroni-textfile-stale.md),
  [config-assertion-failed](config-assertion-failed.md),
  [restore-drill-stale](restore-drill-stale.md) — the dedicated,
  tighter (or, for restore-drill, more patient) alerts this backstop
  defers to.
- `scripts/ci/textfile-producers.manifest` — every producer this
  alert covers.

## Changelog

- **2026-09-25** — created (GH-899): backstop for the 60 producers
  with no dedicated staleness alert.
- **2026-09-27** — excluded `ops_job_*.pid<N>.prom`,
  `ops_job_backfill.prom` and `restore_drill*.prom` after a 14d
  Prometheus review found all three false-firing; added the
  companion `stellarindex_restore_drill_textfile_stale` (35d) for the
  last one.
- **2026-09-28** — excluded `ops_job_usd_volume_restamp.prom`, another
  one-off `stellarindex-ops` job with no timer/cron, same class as
  `ops_job_backfill.prom`.
