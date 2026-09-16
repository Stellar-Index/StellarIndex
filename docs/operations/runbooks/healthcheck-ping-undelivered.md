---
title: Runbook — stellarindex_healthcheck_ping_undelivered
last_verified: 2026-09-16
status: ratified
severity: P3
---

# Runbook — `stellarindex_healthcheck_ping_undelivered`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_healthcheck_ping_undelivered` |
| Severity | P3 |
| Detected by | Prometheus rule in `deploy/monitoring/rules/healthcheck-ping.yml` (and `configs/prometheus/rules.r1/healthcheck-ping.yml`) |
| Typical MTTR | 10 min |
| Impact | None to API consumers. Healthchecks.io notices for the named check stop being evidence about the service. |

## Why this exists

Healthchecks.io decides a check is down by waiting: no ping inside the
period plus grace, and it emails. A service that stopped and a ping that
never left this host reach it identically — as silence — so its email
cannot tell them apart.

Until `configs/healthchecks/hc-ping.sh` this host could not tell them
apart either. Every ping ended in `curl … || true` with the output on
`/dev/null`: a failed delivery produced no journal line, no metric, and
no record that survived the run. An operator holding a "down" email for a
service that was healthy the whole time had nothing to check.

That asymmetry is how a monitoring stack trains its operator to ignore
it. This alert is the other half of the email: while it is firing, a
Healthchecks.io notice for `{{ $labels.check }}` is about this host's
egress, not about the service under the check.

## Symptoms

- `stellarindex_healthcheck_ping_failures_total{check="…"}` has been
  increasing for at least 15 minutes.
- `stellarindex_healthcheck_ping_last_success_unix` for the same check
  has stopped advancing.
- Healthchecks.io may email that the check is down while the service
  behind it is serving normally.

## Quick diagnosis (≤ 5 min)

```sh
# curl's exit code names the layer: 6 = DNS, 7 = connect refused,
# 22 = HTTP error from hc-ping.com, 28 = timeout.
journalctl -u 'stellarindex-*' --since '1 hour ago' | grep hc-ping

# Is the service under the check actually healthy? Answer this BEFORE
# acting on any Healthchecks.io notice for it.
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:3000/v1/healthz
systemctl list-timers 'stellarindex-*' --all

# Can this host reach the pinger at all? (No URL, so no secret.)
curl -fsS -o /dev/null -w '%{http_code}\n' https://hc-ping.com/
```

## Mitigation (≤ 15 min)

- [ ] Confirm the service under the check is healthy — if it is not,
      this alert is secondary; work that check's own runbook first.
- [ ] `rc=6`: DNS. Check `/etc/resolv.conf` and whether the host's
      resolver is reachable.
- [ ] `rc=7` or `rc=28`: egress. Check outbound HTTPS from this host and
      whether hc-ping.com is up (<https://status.healthchecks.io>).
- [ ] `rc=22`: hc-ping.com rejected the request. The most common cause is
      a check that was deleted or regenerated on the dashboard, leaving a
      stale URL in `/etc/default/stellarindex-healthchecks`. Repaste the
      URL from the dashboard; the file is operator-populated and Ansible
      does not overwrite it after first install.
- [ ] Verification: `stellarindex_healthcheck_ping_last_success_unix`
      advances within one timer period (60 s for the heartbeats, 5 min
      for smoke, 15 min for the SLA probe).

## Root cause analysis

Capture the curl exit code and the check name from the journal, and the
value of `stellarindex_healthcheck_ping_failures_total` at the start and
end of the window. A counter that climbed and then went flat on its own
points at hc-ping.com or the network; one that climbs from the moment a
config change landed points at the URL.

## Known false-positive patterns

- A host that has just been reimaged pings with a placeholder URL until
  the operator pastes the real one. `hc_ping` treats an empty URL as
  nothing to deliver and stays silent, but a *wrong* non-empty URL
  produces `rc=22` legitimately.
- Deliberate egress restrictions in a test-net VM. The test-net hosts
  carry no Healthchecks.io URLs, so the series should be absent there
  rather than failing.

## Related

- Implementation: `configs/healthchecks/hc-ping.sh`, sourced by
  `heartbeat.sh`, `smoke.sh` and `sla-probe.sh` in the same directory.
- Unit wiring: `configs/ansible/roles/archival-node/tasks/17-stellarindex-healthchecks.yml`.
- Companion runbooks — the checks whose emails this alert qualifies:
  [`api-smoke-failing.md`](api-smoke-failing.md),
  [`api-smoke-stale.md`](api-smoke-stale.md).
- Tests: `internal/ops/chops/healthcheck_ping_delivery_test.go`.

## Changelog

- 2026-09-16 — initial version, alongside the `hc_ping` wrapper.
