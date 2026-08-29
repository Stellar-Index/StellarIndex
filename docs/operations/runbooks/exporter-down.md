---
title: Runbook — exporter-down
last_verified: 2026-08-29
status: ratified
severity: P1
---

# Runbook — `stellarindex_{redis,postgres,pgbackrest,minio}_exporter_down`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_<exporter>_down` (one per exporter) |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/meta.yml` + `deploy/monitoring/rules/meta.yml` |
| Typical MTTR | 5–15 min |
| Impact | Cascade-blind: every downstream alert that reads metrics from this exporter evaluates `absent_gauge == 0` (no_data) and silently fails to fire. The underlying subsystem can be on fire and we will not be paged. This is the F-0085 failure mode from the 2026-05-10 SEV-2 incident. |

## Why this exists

The 2026-05-10 SEV-2 cascade exposed a structural blindness: the
`stellarindex_redis_writes_blocked` alert watches
`redis_rdb_last_bgsave_status == 0`, but during the cascade
`redis_exporter` itself was down — the metric never reached
Prometheus, the alert evaluated against an absent series, and it
never fired. F-0085 in the 2026-05-26 audit generalised the gap to
every exporter whose absence would silence a downstream alert
family. These meta-alerts surface the blindness directly.

## Symptoms

- Prometheus shows `up{job="<exporter>"} == 0` or the series is
  absent entirely for 2+ minutes.
- Downstream alerts that depend on this exporter's metrics are
  unexpectedly silent (no firing, no resolution events) — they are
  not OK, they are not evaluating.
- Dashboards keyed on this exporter's metrics go flat.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Confirm which exporter — look at the alert's job label, then:
ssh root@136.243.90.96
systemctl status prometheus-redis-exporter      # redis
systemctl status prometheus-postgres-exporter   # postgres
systemctl status pgbackrest_exporter            # pgbackrest
systemctl status minio                          # minio (server itself)

# 2. Tail the unit's recent logs for crash / auth / config errors.
journalctl -u <unit> -n 200 --no-pager

# 3. Hit the exporter directly from r1 — does it answer at all?
curl -sf http://localhost:9121/metrics | head -5   # redis_exporter
curl -sf http://localhost:9187/metrics | head -5   # postgres_exporter
curl -sf http://localhost:9854/metrics | head -5   # pgbackrest_exporter
curl -sf -H "Authorization: Bearer $(cat /etc/prometheus/minio.token)" \
     http://localhost:9000/minio/v2/metrics/cluster | head -5   # minio
```

## Mitigation (≤ 15 min)

- [ ] Restart the failed exporter: `systemctl restart <unit>`.
- [ ] If the unit refuses to start, read the journal — common causes
      are a stale auth credential (postgres exporter DSN drift,
      MinIO bearer-token file missing or rotated), a permissions
      regression on the read socket / data dir, or the underlying
      service it scrapes being itself down.
- [ ] Verify recovery: `curl` the exporter (commands above) returns
      `200` with metrics. In Prometheus, `up{job="<job>"}` returns
      `1`. The meta-alert auto-resolves within ~2 min.
- [ ] Confirm dependent alert family is no longer absent — e.g. for
      redis, check `redis_rdb_last_bgsave_status` is present and
      `== 1`.

## Per-exporter notes

All three of r1's exporters are installed by the ONE task file
`configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml`
(groups A/B/C). The `redis-sentinel` role's own
`tasks/07-monitoring.yml` ships a *different*, tarball-based
redis_exporter for the Sentinel-cluster boxes — that role is **not
applied to r1** (`playbooks/archival-node.yml` runs `archival-node`
only), so do not go looking there when an r1 exporter misbehaves.

- **redis_exporter** — Debian unit `prometheus-redis-exporter`,
  port 9121 (group A; bound to `127.0.0.1` via
  `WEB_LISTEN_ADDRESS` in `/etc/default/prometheus-redis-exporter`).
  Feeds `cache.yml` + `stellarindex_redis_writes_blocked`.
- **postgres_exporter** — Debian unit `prometheus-postgres-exporter`,
  port 9187 (group B; bound to `127.0.0.1` via
  `PG_EXPORTER_WEB_LISTEN_ADDRESS`). Reads `DATA_SOURCE_NAME` from
  `/etc/default/prometheus-postgres-exporter` — peer auth on the
  local Unix socket as the package's own `prometheus` role, which
  needs `pg_monitor`. Feeds the `pg_*` alerts in `storage.yml`.
- **pgbackrest_exporter** — unit `pgbackrest_exporter`, port 9854
  (group C; pinned upstream tarball, no Debian package — the role
  fails fast unless `pgbackrest_exporter_release_sha256` is set in
  inventory). Feeds `stellarindex_timescale_backup_failed` +
  `stellarindex_timescale_backup_none_24h`.
- **minio** — unit `minio`, port 9000. Bearer token at
  `/etc/prometheus/minio.token`; rotate via
  `mc admin prometheus generate`. Feeds galexie-archive +
  archive-completeness families.

## Root cause analysis

Capture for postmortem:

- Unit journal from window before the alert fired
  (`journalctl -u <unit> --since "30 min ago"`).
- The exporter's own metrics-endpoint response immediately before
  recovery (curl).
- Any concurrent disk / memory pressure on r1 (`df -h`, `free -m`,
  `dmesg -T | tail -50`).
- Whether the underlying service (redis / postgres / minio) was also
  down — if so, treat this as an indicator and follow that service's
  runbook for the real RCA.

## Known false-positive patterns

- Brief restarts during planned exporter upgrades — silence the
  alert for the upgrade window via amtool.
- Bearer-token rotation for MinIO without restarting Prometheus —
  Prometheus must reload its bearer-token file.

## Related

- F-0085 (audit-2026-05-26) — the audit finding that motivated
  this alert family.
- 2026-05-10 SEV-2 postmortem — the original incident that exposed
  the blindness.
- `docs/operations/runbooks/redis-write-blocked-disk-full.md` — the
  downstream alert that was silenced.
- `docs/operations/runbooks/scrape-failing.md` — companion
  generalised scrape-failure runbook.

## Changelog

- 2026-05-27 — initial draft (F-0085 audit response).
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). Corrected the per-exporter install path: all three r1
  exporters come from `archival-node/tasks/16-prometheus-exporters.yml`,
  NOT the redis-sentinel role (which r1 never runs). Host shape →
  r1's IP.
