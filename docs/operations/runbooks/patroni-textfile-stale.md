---
title: Runbook — patroni-textfile-stale
last_verified: 2026-09-22
status: current
severity: P3
---

# Runbook — `stellarindex_patroni_textfile_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_patroni_textfile_stale` |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`; producer is `patroni-textfile-scraper.timer` (30s, patroni role `11-monitoring.yml`) running `/usr/local/bin/patroni-textfile-scraper` |
| Typical MTTR | 5–15 min |
| Impact | `stellarindex_patroni_role` / `stellarindex_patroni_running` are frozen at their last-reported value, not live cluster state — a real role change (failover) during the outage is invisible to anything reading these gauges. |

## Why this exists

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

## Diagnosis

```sh
systemctl status patroni-textfile-scraper.timer
systemctl status patroni-textfile-scraper.service
journalctl -u patroni-textfile-scraper.service -n 50
/usr/local/bin/patroni-textfile-scraper   # run by hand, inspect the error
```

Common causes: `curl` to `127.0.0.1:8008/cluster` failing (patroni's
REST API down), `jq` missing, or the timer itself disabled by an
ansible re-apply that didn't restart it.

## Related

- [config-assertion-failed](config-assertion-failed.md) — the alert
  this pattern is modelled on.
- `configs/ansible/roles/patroni/tasks/11-monitoring.yml` — scraper,
  service and timer definitions.

## Changelog

- **2026-09-22** — created (T586): patroni.prom was one of four
  manifest producers with no staleness guard of any kind.
