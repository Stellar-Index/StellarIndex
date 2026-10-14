---
title: Runbook — prometheus-down (TSDB corruption)
last_verified: 2026-08-29
status: current
severity: P1
---

# Runbook — `prometheus_down` (TSDB head-chunk corruption)

## At a glance

P1. No `prometheus_down` rule exists. Detection is the `stellarindex_deadmansswitch` heartbeat going silent at Healthchecks.io ([meta.md#stellarindex_deadmansswitch](meta.md#stellarindex_deadmansswitch)). While down, no metrics are collected and every metric alert is deaf; `/v1/status` returns degraded. Usually follows a disk-full or OOM during a write to `/var/lib/prometheus/metrics2/chunks_head/` (same family as [`cache.md#stellarindex_redis_writes_blocked`](cache.md#stellarindex_redis_writes_blocked); post-mortem: `internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`).

## Check

```sh
ssh root@136.243.90.96 systemctl status prometheus --no-pager | head -10
ssh root@136.243.90.96 journalctl -u prometheus -n 50 --no-pager | grep -E 'level=error|exited|corruption'
ssh root@136.243.90.96 df -h /var/lib/prometheus
```

`corruption in head chunk file` -> A. OOM / WAL replay panic -> B. `df` over 95 % -> free disk first (see the cache runbook).

## Mitigation A - head-chunk corruption

Prometheus auto-deletes corrupt head chunks on start; a bare retry usually works.

1. `systemctl start prometheus`; `journalctl -u prometheus -f` until `Server is ready to receive web requests.`
2. `curl -sS localhost:9090/-/ready` returns `Prometheus Server is Ready.` within 30 s.
3. If the same error repeats, move the named chunk aside (loses ~2 h of head data; persisted blocks `metrics2/01*` are untouched):

```sh
systemctl stop prometheus
ls -la /var/lib/prometheus/metrics2/chunks_head/  # find the file matching the journal error
mv /var/lib/prometheus/metrics2/chunks_head/000048 /tmp/prom-corrupt-000048-$(date +%Y%m%d).bak
systemctl start prometheus
```

## Mitigation B - WAL or block corruption

Do not restore from backup; Prometheus is not a source of truth.

1. `systemctl stop prometheus`
2. `mv /var/lib/prometheus/metrics2/wal /tmp/prom-wal-$(date +%Y%m%d).bak`
3. `systemctl start prometheus` (rebuilds an empty WAL; loses ~2 h). Verify as in A.
4. If it still fails, rebuild the TSDB (loses all history):

```sh
systemctl stop prometheus
mv /var/lib/prometheus/metrics2 /var/lib/prometheus/metrics2.bak-$(date +%Y%m%d)
mkdir /var/lib/prometheus/metrics2
chown prometheus:prometheus /var/lib/prometheus/metrics2
systemctl start prometheus
```

## Permanent fix - auto-restart

The packaged unit has `Restart=on-abnormal`, which ignores exit-code failures. r1 runs the apt binary (`/usr/bin/prometheus`), so the Ansible role template does not apply. This drop-in is a sanctioned out-of-band exception (`configs/ansible/playbooks/monitoring.yml` is not runnable against `inventory/r1.yml`), not covered by `ansible-drift.yml`, and survives apt upgrades:

```sh
mkdir -p /etc/systemd/system/prometheus.service.d
cat > /etc/systemd/system/prometheus.service.d/auto-restart.conf <<'EOF'
[Service]
Restart=on-failure
RestartSec=5s
EOF
systemctl daemon-reload
systemctl restart prometheus
```

## False positives

- `systemctl restart prometheus` pauses the heartbeat ~5 s, inside the grace period.
- A broken Alertmanager `deadmansswitch` receiver URL (`configs/alertmanager/alertmanager.r1.yml`) makes Healthchecks.io report silence while Prometheus is healthy; check Alertmanager's notification log first.

For a postmortem, capture the last 200 journal lines, `df -h` / `du -sh /var/lib/prometheus/*`, the OOM killer in `/var/log/syslog`, and keep one corrupt chunk for an upstream bug report.

Escalate: if Prometheus is not ready after the TSDB rebuild, page the infra owner.

## Related

- [Alerts catalogue](../alerts-catalog.md)
