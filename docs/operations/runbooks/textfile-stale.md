---
title: Runbook — textfile-stale
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — textfile-stale alerts

## At a glance

Rules: `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`. node_exporter re-serves a textfile's last values forever, so a dead producer freezes its gauges silently; these alerts watch `node_textfile_mtime_seconds`.

## stellarindex_textfile_producer_stale

P3. A `.prom` file under the textfile collector has not been rewritten for 24 h. The rule has no `file=` selector, so it backstops every producer in `scripts/ci/textfile-producers.manifest` that lacks a tighter dedicated alert ([patroni-textfile-stale](#stellarindex_patroni_textfile_stale), [config-assertion-failed](config-assertion-failed.md), [restore-drill-stale](restore-drill.md#stellarindex_restore_drill_stale) at 35 d). `scripts/ci/lint_textfile_exposition.py` fails CI for a producer with no rule.

Excluded by design (not dead producers): `ops_job_*.pid<N>.prom` (orphans), `ops_job_backfill.prom`, `ops_job_usd_volume_restamp.prom` and `ops_job_projected_rebuild_<source>.prom` (one-off jobs; a run in progress is covered by `stellarindex_ops_job_heartbeat_stale`), `restore_drill*.prom`, and `heavy_job_*.prom`. Lock-gated producers (`archive_completeness.prom`, `verify_archive_tier_a.prom`, `verify_archive_tier_b.prom`, `lake_verify.prom`, `network_state_verify.prom`, `ops_job_asset_registry_backfill.prom`) are dropped while `stellarindex_heavy_lock_held_since_unix` shows the heavy lock held under 72 h; the regex in the rule's `unless` clause is the one list. A lock held past 72 h, or a gated producer with no lock held, still alerts.

Check:

```sh
# $labels.file is the full path, e.g. /var/lib/node_exporter/textfile_collector/ch_schema_snapshot.prom
grep -F "$(basename "$file")" scripts/ci/textfile-producers.manifest
systemctl status <the producer's timer/service>
journalctl -u <the producer's service> -n 50
fuser -v /run/lock/stellarindex-heavy.lock   # stuck heavy-lock holder
```

Likely causes:

- The producer timer was disabled by an ansible re-apply that did not restart it.
- The producer script errors before its atomic `mv` of the rendered file.
- A dependency it queries (patroni REST, ClickHouse, S3/MinIO) is unreachable.
- A heavy job holds the lock past 72 h.

Fix: restart the timer or fix the dependency, run the producer once by hand, confirm `node_textfile_mtime_seconds` advances.

Escalate: if the producer cannot be identified from the manifest, page the infra owner.

## stellarindex_patroni_textfile_stale

P3. `patroni.prom` has not been rewritten. Producer: `patroni-textfile-scraper.timer` (30 s; `configs/ansible/roles/patroni/tasks/11-monitoring.yml`) running `/usr/local/bin/patroni-textfile-scraper`. Until fixed, `stellarindex_patroni_role` / `stellarindex_patroni_running` are frozen and a failover is invisible to them.

Check:

```sh
systemctl status patroni-textfile-scraper.timer
systemctl status patroni-textfile-scraper.service
journalctl -u patroni-textfile-scraper.service -n 50
/usr/local/bin/patroni-textfile-scraper   # run by hand, inspect the error
```

Likely causes:

- `curl` to `127.0.0.1:8008/cluster` fails (patroni REST API down).
- `jq` missing.
- The timer was disabled by an ansible re-apply that did not restart it.

Fix: fix the cause, `systemctl restart patroni-textfile-scraper.timer`, confirm the mtime advances.

Escalate: if patroni's REST API is down, go to [postgres.md](postgres.md#stellarindex_timescale_primary_down).

## Related

- [Alerts catalogue](../alerts-catalog.md)
