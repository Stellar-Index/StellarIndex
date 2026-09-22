---
title: Runbook — keepalived-textfile-stale
last_verified: 2026-09-22
status: current
severity: P3
---

# Runbook — `stellarindex_keepalived_textfile_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_keepalived_textfile_stale` |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/infra.yml` + `configs/prometheus/rules.r1/infra.yml`; producer is `keepalived-textfile-scraper.timer` (30s, haproxy role `07-monitoring.yml`) running `/usr/local/bin/keepalived-textfile-scraper` |
| Typical MTTR | 5–15 min |
| Impact | `stellarindex_keepalived_active` / `stellarindex_haproxy_vip_owner` are frozen at their last-reported value, not live VRRP state — a real VIP failover during the outage is invisible to anything reading these gauges. |

## Why this exists

node_exporter re-serves a textfile's last written values on every
scrape regardless of whether the process that writes it is still
running. If `keepalived-textfile-scraper.timer` dies,
`keepalived.prom` stops being rewritten but keeps being scraped, so
`stellarindex_keepalived_active`/`stellarindex_haproxy_vip_owner`
alone can never detect the scraper's own death. This alert watches
`node_textfile_mtime_seconds{file="keepalived.prom"}` directly: the
file's real mtime stops advancing the moment the timer stops
rewriting it, same mechanism as
`stellarindex_config_assertions_stale`.

## Diagnosis

```sh
systemctl status keepalived-textfile-scraper.timer
systemctl status keepalived-textfile-scraper.service
journalctl -u keepalived-textfile-scraper.service -n 50
/usr/local/bin/keepalived-textfile-scraper   # run by hand, inspect the error
```

Common causes: `systemctl is-active keepalived` failing (keepalived
itself not installed/running on this host), or the `ip -4 a show`
interface name no longer matching `keepalived_iface`.

## Related

- [config-assertion-failed](config-assertion-failed.md) — the alert
  this pattern is modelled on.
- `configs/ansible/roles/haproxy/tasks/07-monitoring.yml` — scraper,
  service and timer definitions.

## Changelog

- **2026-09-22** — created (T586): keepalived.prom was one of four
  manifest producers with no staleness guard of any kind.
