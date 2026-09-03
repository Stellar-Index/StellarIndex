---
title: Runbook — alertmanager-down
last_verified: 2026-09-03
status: current
severity: P1
---

# Runbook — `stellarindex_alertmanager_down`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_alertmanager_down` |
| Severity | page |
| Fires when | `up{job="alertmanager"} == 0`, or the series is absent for 10 min, sustained 5 min |
| Means | no alert in the system can be delivered, including this one |

Until 2026-09-03 there was **no rule at any severity** on
`up{job="alertmanager"}`. The four exporters each had a page with an
`absent_over_time` guard; the notification layer itself had nothing.

## 1. Triage

```sh
ssh root@136.243.90.96
systemctl status prometheus-alertmanager    # NB: not `alertmanager` — that unit is inactive
journalctl -u prometheus-alertmanager -n 80 --no-pager
ss -lntp | grep 9093
```

The unit name is `prometheus-alertmanager.service`. A plain
`systemctl status alertmanager` reports an inactive unit that has never
been the running one, which reads as a false confirmation.

## 2. Common causes

| Symptom in the journal | Cause | Action |
| --- | --- | --- |
| `error loading config` + exit | bad config installed by hand | restore `/etc/prometheus/alertmanager.yml` from the last good copy, then re-apply from a checkout |
| OOM-killed | host memory pressure | check `stellarindex_host_memory_high`; Alertmanager's own footprint is small, so look for the real consumer |
| Port 9093 already bound | a stale process survived a restart | `systemctl stop`, confirm with `ss -lntp`, then start |
| Scrape target absent, unit healthy | Prometheus scrape config lost the job | check `job_name: alertmanager` in `/etc/prometheus/prometheus.yml` |

That last row is why the `absent_over_time` arm exists: if the job is
dropped from service discovery, `up{...} == 0` alone matches nothing
and the alert would be silently unfireable.

## 3. Recover

```sh
systemctl restart prometheus-alertmanager
curl -s localhost:9093/api/v2/status | head -c 300
```

Then verify delivery actually resumed — a running process is not proof
of fan-out:

```sh
curl -s 'localhost:9090/api/v1/query?query=rate(alertmanager_notifications_total\{integration="webhook"\}[5m])*3600'
```

If the process is up but that stays at zero, you have the *other*
failure — see [alertmanager-not-notifying](alertmanager-not-notifying.md).

## Related

- [alertmanager-not-notifying](alertmanager-not-notifying.md)
- [alertmanager-bad-config](alertmanager-bad-config.md)
- [deadmansswitch](deadmansswitch.md)
