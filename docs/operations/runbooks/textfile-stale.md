---
title: Runbook — textfile-stale
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — textfile-stale alerts

Textfile-collector staleness alerts. Merged from two former pages.

## At a glance

- [`stellarindex_textfile_producer_stale`](#stellarindex_textfile_producer_stale)
- [`stellarindex_patroni_textfile_stale`](#stellarindex_patroni_textfile_stale)

## stellarindex_textfile_producer_stale

_Source page `textfile-stale.md#stellarindex_textfile_producer_stale`: status current, severity P3, last verified 2026-09-25._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_textfile_producer_stale` |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`; matches `node_textfile_mtime_seconds` with no `file=` selector |
| Typical MTTR | 5–30 min |
| Impact | Every family in the stale `.prom` file is frozen at its last-written value; node_exporter keeps re-serving it, so nothing that only reads those series would otherwise notice. |

### Why this exists

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

**2026-10-02: `ops_job_projected_rebuild_<source>.prom` added** — the
`projected-rebuild` job names its heartbeat per source, so every source
ever rebuilt leaves a file behind with `stellarindex_ops_job_running 0`.
A run in progress is covered by `stellarindex_ops_job_heartbeat_stale`.

**2026-10-05: lock-gated producers deferred while the heavy lock is held.**
`archive_completeness.prom`, `verify_archive_tier_a.prom`,
`verify_archive_tier_b.prom`, `lake_verify.prom`, `network_state_verify.prom`
and `ops_job_asset_registry_backfill.prom` wait behind (or exit 75 on) the
host-wide heavy-job lock, so a long heavy job (a long projector-replay or recompress run) froze
them past 24h and the alert fired for healthy producers. The rule now
drops them while `stellarindex_heavy_lock_held_since_unix` (written by
`run-heavy-job.sh` to `heavy_job_<name>.prom` for the job's lifetime,
removed at exit) shows the lock held under 72h. The regex in the rule's
`unless` clause is the one list of gated producers. A lock held past 72h,
or a gated producer with no lock held, still alerts: check
`fuser -v /run/lock/stellarindex-heavy.lock` for a stuck holder. The
`heavy_job_*.prom` files are excluded from the catch-all themselves. A
non-root wrapper launch execs the payload and does not publish the metric.

### Diagnosis

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

### Changelog

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
- **2026-10-02** — excluded `ops_job_projected_rebuild_<source>.prom`,
  the per-source one-shot rebuild heartbeats.

## stellarindex_patroni_textfile_stale

_Source page `textfile-stale.md#stellarindex_patroni_textfile_stale`: status current, severity P3, last verified 2026-09-22._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_patroni_textfile_stale` |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`; producer is `patroni-textfile-scraper.timer` (30s, patroni role `11-monitoring.yml`) running `/usr/local/bin/patroni-textfile-scraper` |
| Typical MTTR | 5–15 min |
| Impact | `stellarindex_patroni_role` / `stellarindex_patroni_running` are frozen at their last-reported value, not live cluster state — a real role change (failover) during the outage is invisible to anything reading these gauges. |

### Why this exists

node_exporter re-serves a textfile's last written values on every
scrape regardless of whether the process that writes it is still
running. If `patroni-textfile-scraper.timer` dies, `patroni.prom`
stops being rewritten but keeps being scraped, so
`stellarindex_patroni_role`/`_running` alone can never detect the
scraper's own death — only a change it reported before dying. This
alert watches `node_textfile_mtime_seconds{file="patroni.prom"}`
directly: the file's real mtime stops advancing the moment the timer
stops rewriting it, same mechanism as
`stellarindex_config_assertions_stale`.

### Diagnosis

```sh
systemctl status patroni-textfile-scraper.timer
systemctl status patroni-textfile-scraper.service
journalctl -u patroni-textfile-scraper.service -n 50
/usr/local/bin/patroni-textfile-scraper   # run by hand, inspect the error
```

Common causes: `curl` to `127.0.0.1:8008/cluster` failing (patroni's
REST API down), `jq` missing, or the timer itself disabled by an
ansible re-apply that didn't restart it.

### Changelog

- **2026-09-22** — created (T586): patroni.prom was one of four
  manifest producers with no staleness guard of any kind.

## Related

**stellarindex_textfile_producer_stale**

- [patroni-textfile-stale](textfile-stale.md#stellarindex_patroni_textfile_stale),
  [config-assertion-failed](config-assertion-failed.md),
  [restore-drill-stale](restore-drill.md#stellarindex_restore_drill_stale) — the dedicated,
  tighter (or, for restore-drill, more patient) alerts this backstop
  defers to.
- `scripts/ci/textfile-producers.manifest` — every producer this
  alert covers.

**stellarindex_patroni_textfile_stale**

- [config-assertion-failed](config-assertion-failed.md) — the alert
  this pattern is modelled on.
- `configs/ansible/roles/patroni/tasks/11-monitoring.yml` — scraper,
  service and timer definitions.
