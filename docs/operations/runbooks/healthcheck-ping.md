---
title: Runbook — Healthchecks.io ping delivery
last_verified: 2026-10-06
status: ratified
severity: P3
---

# Runbook — Healthchecks.io ping delivery

Healthchecks.io marks a check down by silence, so a stopped service and a
ping that never left this host look identical to it. `configs/healthchecks/hc-ping.sh`
(sourced by `heartbeat.sh`, `smoke.sh`, `sla-probe.sh` in that directory)
records every failed delivery in the journal and in node_exporter's textfile
collector (`/var/lib/node_exporter/textfile_collector/hc_ping_<check>.prom`).
While the alert fires, a Healthchecks.io "down" notice for that check is about
this host's egress, not the service.

Impact: none to API consumers. Severity ticket (P3), typical MTTR 10 min.
Wiring: `configs/ansible/roles/archival-node/tasks/17-stellarindex-healthchecks.yml`.
Test: `internal/ops/chops/healthcheck_ping_delivery_test.go`.
Companion runbooks (the checks whose emails this qualifies):
[`api-smoke.md#stellarindex_api_smoke_failing`](api-smoke.md#stellarindex_api_smoke_failing),
[`api-smoke.md#stellarindex_api_smoke_stale`](api-smoke.md#stellarindex_api_smoke_stale).

## At a glance

- [`stellarindex_healthcheck_ping_undelivered`](#stellarindex_healthcheck_ping_undelivered)

## stellarindex_healthcheck_ping_undelivered

Trips (identical in `deploy/monitoring/rules/healthcheck-ping.yml` and
`configs/prometheus/rules.r1/healthcheck-ping.yml`), severity `ticket`:

```
increase(stellarindex_healthcheck_ping_failures_total[15m]) > 0
and
(time() - stellarindex_healthcheck_ping_last_success_unix) > (15 * 60)
for: 5m
```

Two clauses so one lost ping stays silent: a blip that `hc_ping`'s own
`--retry 2` did not absorb but that cleared on the next timer firing meets the
first clause only. A check that never delivered has `last_success = 0`, so the
second clause is true by construction (a wrong URL from install is reported).

Symptoms: `stellarindex_healthcheck_ping_failures_total{check="…"}` rising for
at least 15 min, `stellarindex_healthcheck_ping_last_success_unix` for that
check not advancing, and possibly a Healthchecks.io "down" email while the
service is fine.

Diagnose (≤ 5 min):

```sh
# curl's exit code names the layer: 6 = DNS, 7 = connect refused,
# 22 = HTTP error from hc-ping.com, 28 = timeout.
journalctl -u 'stellarindex-*' --since '1 hour ago' | grep hc-ping

# Is the service under the check actually healthy? Answer BEFORE acting on
# any Healthchecks.io notice for it.
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:3000/v1/healthz
systemctl list-timers 'stellarindex-*' --all

# Can this host reach the pinger at all? (No URL, so no secret.)
curl -fsS -o /dev/null -w '%{http_code}\n' https://hc-ping.com/
```

Fix (≤ 15 min):

- Service under the check unhealthy: this alert is secondary; work that
  check's own runbook first.
- `rc=6` DNS: check `/etc/resolv.conf` and that the host resolver is reachable.
- `rc=7` or `rc=28` egress: check outbound HTTPS and whether hc-ping.com is up
  (<https://status.healthchecks.io>).
- `rc=22`: hc-ping.com rejected the request. Usual cause is a check deleted or
  regenerated on the dashboard, leaving a stale URL in
  `/etc/default/stellarindex-healthchecks`. Repaste the URL from the dashboard;
  the file is operator-populated and Ansible does not overwrite it after first
  install.
- Verify: `stellarindex_healthcheck_ping_last_success_unix` advances within one
  timer period (60 s heartbeats, 5 min smoke, 15 min SLA probe).

Root cause: capture the curl exit code and check name from the journal, plus
`stellarindex_healthcheck_ping_failures_total` at the start and end of the
window. Climbed then flat on its own = hc-ping.com or network; climbing from a
config change = the URL.

False positives:

- A just-reimaged host: `hc_ping` treats an empty URL as nothing to deliver and
  stays silent, but a wrong non-empty URL gives `rc=22` legitimately.
- Test-net VMs carry no Healthchecks.io URLs, so the series should be absent
  there, not failing.

## Related

- [Alerts catalogue](../alerts-catalog.md)
