---
title: Runbook — scrape-failing
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_prometheus_scrape_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_prometheus_scrape_failing` |
| Severity | P3 (`severity: informational`) |
| Detected by | `configs/prometheus/rules.r1/meta.yml` (group `stellarindex.meta`, `severity: informational`, `for: 2m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/meta.yml`. NOTE the deliberate job-name split (F-1222): the r1 rule's regex uses the HYPHENATED job names of `prometheus.r1.yml` (`stellarindex-api\|stellarindex-indexer\|stellarindex-aggregator\|...`), the multi-host twin the UNDERSCORED ones (`stellarindex_api\|...`). |
| Typical MTTR | 5–30 min |
| Impact | We've lost visibility into some subsystem. Doesn't mean the subsystem is unhealthy — often the exporter is the problem, and the service it monitors is fine. But we can't *tell* which is true until we investigate. |

## Symptoms

- `up{job=<J>, instance=<I>} == 0` for ≥ 2 min, for the jobs in
  the rule's regex (on r1: `stellarindex-api`,
  `stellarindex-indexer`, `stellarindex-aggregator`,
  `node_exporter`, `prometheus`, `caddy`, `galexie`).
- Gap in the service's metric graphs from 2 min ago to now.
- The service's own user-visible health may be fine.

**Deliberately NOT covered here (r1):** the four exporter jobs
`redis_exporter` / `postgres_exporter` / `pgbackrest_exporter` /
`minio` are excluded from this alert's regex on purpose — each has
its own dedicated P1 `*_exporter_down` meta-alert (F-0085: an
exporter outage silently blinds every alert that depends on its
metrics, so those page rather than filing an informational). See
[exporter-down.md](exporter-down.md).

## Quick diagnosis (≤ 5 min)

On r1 Prometheus runs ON THE ARCHIVAL HOST ITSELF as the distro's
`prometheus.service` unit — there are no `mon-01` / `mon-02`
monitoring hosts. Its scrape config is
`configs/prometheus/prometheus.r1.yml` (installed to
`/etc/prometheus/`); every target is a `localhost:<port>` on the
same box.

```sh
# What's Prometheus's view of the failing target?
ssh root@136.243.90.96 "curl -s http://localhost:9090/api/v1/targets?state=active" | \
  jq '.data.activeTargets[] | select(.health != "up") |
        {job: .labels.job, instance: .labels.instance, lastError: .lastError}'

# Is the /metrics endpoint on the target reachable locally?
ssh root@136.243.90.96 "curl -s http://localhost:<port>/metrics | head"

# Is the exporter unit alive? (redis_exporter, postgres_exporter,
# node_exporter all ship as systemd units via their ansible roles.
# List what's actually installed first, then status the one you
# need — exporter unit names vary by package.)
ssh root@136.243.90.96 "systemctl list-units | grep -i exporter"
ssh root@136.243.90.96 "systemctl status <exporter-unit> --no-pager | head -15"
```

The `lastError` field from the Prometheus API tells you exactly
why scrape failed: connection refused, TLS, 404 on the /metrics
path, parse error, etc.

## Typical root causes

1. **Target host rebooted or the unit restarted**. Common during
   ansible-driven upgrades; Prometheus's static-config discovery
   re-resolves on each scrape so target reappearance is
   bounded by the scrape interval. The alert `for: 2m` absorbs
   this — if it fires, the host or unit is staying down.

2. **Exporter crash**. `redis_exporter`, `postgres_exporter`,
   `node_exporter` each have their own failure modes.
   - Mitigation: `ssh root@<host> "systemctl restart <exporter>"`.

3. **Static-config drift**. A new host added without an
   accompanying entry in the prometheus role's `prometheus.yml.j2`
   inventory. Target was added to inventory but role hasn't been
   re-applied; or the inverse — host removed from inventory but
   prometheus config still scrapes it.

4. **Auth drift**. Some exporters require a basic-auth or bearer
   token; if the credentials in vault got rotated without
   re-applying the prometheus role to refresh the scrape config,
   prometheus gets 401 on every attempt.

5. **Firewall / nftables rule blocking Prometheus.** On r1 every
   scrape is localhost→localhost, so this takes a host-local nft
   change to bite; in the multi-host shape, a new rule on the
   target host (or the colo perimeter) doesn't allow the
   monitoring hosts in to the metrics port.

## Mitigation

- [ ] Step 1 — look at `lastError` — that usually points at the
      exact cause.
- [ ] Step 2 — fix per cause:
      - Exporter crash: `systemctl restart <exporter>` on the host.
      - Static-config drift: r1 — install the updated
        `configs/prometheus/prometheus.r1.yml` to
        `/etc/prometheus/` and SIGHUP the unit; multi-host —
        re-apply the `prometheus` ansible role (rolls
        `/etc/prometheus/prometheus.yml` and SIGHUPs the unit).
      - Auth: rotate vault entry, re-apply role.
      - Firewall: on r1 all scrapes are localhost — a firewall
        cause means a host-local nft change; multi-host: open
        ingress from the monitoring hosts to the target metrics
        port.
- [ ] Step 3 — if it's genuinely the *target service* down, not
      the scrape, cross-reference with that service's own alerts.
- [ ] Verification: `up` returns to 1; metrics resume flowing.

## Known false-positive patterns

- **Prometheus reload during a config change** drops all
  targets briefly. `for: 2m` is chosen to avoid paging on this.
- **Cold-start scrape window after a unit restart** — between
  `systemctl start` and the first /metrics serve, Prometheus
  sees `up==0`. Resolves on the next scrape interval.

## Related

- `alertmanager-bad-config.md` — AlertManager-specific reload
  issues.
- `exporter-down.md` — the dedicated P1 meta-alerts for the four
  exporter jobs deliberately excluded from this alert's regex
  (F-0085).
- `deadmansswitch.md` — the failover when Prometheus itself is down.
- Per-service runbooks if the service is actually down, not just
  unscrapeable.

## Changelog

- 2026-04-23 — initial draft.
- 2026-05-02 — diagnosis converted from kubectl `prometheus-0`
  pod-exec / pod-list to the `mon-01` / `mon-02` hosts running
  `prometheus.service` per the `prometheus` ansible role
  (ADR-0008). Service-discovery section rewritten to talk about
  static-config drift instead of ServiceMonitor / PodMonitor.
- 2026-08-29 — re-verified against HEAD: mon-01/mon-02 fiction
  replaced with r1 reality (Prometheus runs on the archival host
  as the distro unit; scrape config
  `configs/prometheus/prometheus.r1.yml`; all-localhost targets),
  F-1222 job-name split noted in Detected-by, the four exporter
  jobs' deliberate exclusion documented (F-0085 →
  exporter-down.md), per-tree re-apply instructions, exporter
  status glob replaced with list-then-status, dual-tree
  Detected-by. Status draft → current.
