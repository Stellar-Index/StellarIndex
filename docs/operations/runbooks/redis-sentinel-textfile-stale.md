---
title: Runbook — redis-sentinel-textfile-stale
last_verified: 2026-09-22
status: current
severity: P3
---

# Runbook — `stellarindex_redis_sentinel_textfile_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_redis_sentinel_textfile_stale` |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/cache.yml` + `configs/prometheus/rules.r1/cache.yml`; producer is `redis-sentinel-textfile-scraper.timer` (30s, redis-sentinel role `07-monitoring.yml`) running `/usr/local/bin/redis-sentinel-textfile-scraper` |
| Typical MTTR | 5–15 min |
| Impact | `stellarindex_redis_sentinel_primary` is frozen at its last-reported value, not live Sentinel state — a real primary re-election during the outage is invisible to anything reading this gauge. |

## Why this exists

node_exporter re-serves a textfile's last written values on every
scrape regardless of whether the process that writes it is still
running. If `redis-sentinel-textfile-scraper.timer` dies,
`redis_sentinel.prom` stops being rewritten but keeps being scraped,
so `stellarindex_redis_sentinel_primary` alone can never detect the
scraper's own death. This alert watches
`node_textfile_mtime_seconds{file="redis_sentinel.prom"}` directly:
the file's real mtime stops advancing the moment the timer stops
rewriting it, same mechanism as
`stellarindex_config_assertions_stale`.

## Diagnosis

```sh
systemctl status redis-sentinel-textfile-scraper.timer
systemctl status redis-sentinel-textfile-scraper.service
journalctl -u redis-sentinel-textfile-scraper.service -n 50
/usr/local/bin/redis-sentinel-textfile-scraper   # run by hand, inspect the error
```

Common causes: `REDIS_PASSWORD` missing from
`/etc/default/redis_exporter` (`EnvironmentFile=-`, so the unit
starts but the scraper fails auth), or `redis-cli SENTINEL
get-master-addr-by-name` failing because Sentinel itself is down.

## Related

- [config-assertion-failed](config-assertion-failed.md) — the alert
  this pattern is modelled on.
- [redis-master-down](redis-master-down.md) — Sentinel topology
  background (not deployed on r1 today).
- `configs/ansible/roles/redis-sentinel/tasks/07-monitoring.yml` —
  scraper, service and timer definitions.

## Changelog

- **2026-09-22** — created (T586): redis_sentinel.prom was one of
  four manifest producers with no staleness guard of any kind.
