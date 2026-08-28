---
title: Runbook — host-down
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_host_down`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_host_down` |
| Severity | P2 (`severity: ticket` → Discord #stellarindex-alerts). Escalate manually to a page if the box is really down — see Step 3. |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (loaded on r1 from `/etc/prometheus/rules.r1/*.yml`; `deploy/monitoring/rules/infra.yml` is the multi-host twin with the same expr) |
| Typical MTTR | 5 min (exporter restart) – hours (hardware, via Hetzner Robot) |
| Impact | `up{job="node_exporter"} == 0` for 2+ min. **On r1 this alert can only fire while the host is alive** — Prometheus and Alertmanager run on r1 itself (`configs/prometheus/prometheus.r1.yml` → `localhost:9093`), so if the box is genuinely down nothing on it can page you. A real host-down surfaces through the out-of-band Healthchecks.io checks instead (see Symptoms). |

## Symptoms

- `up{job="node_exporter",instance="localhost:9100"} == 0` for ≥ 2 min
  (single scrape target on r1, label `host=r1`).
- `stellarindex_prometheus_scrape_failing` (`rules.r1/meta.yml`,
  informational) fires alongside for the same target — expected.
- If you were paged by `stellarindex_host_down` itself: Prometheus is
  evaluating rules, therefore r1 is up. It is the exporter or the
  scrape path, not the box.
- A **genuine r1 outage** looks like this instead — Healthchecks.io
  checks going red, roughly in this order:
  - `stellarindex-heartbeat@{indexer,aggregator,api}.timer` (60 s
    cadence, `configs/healthchecks/stellarindex-heartbeat@.timer`);
  - `stellarindex_deadmansswitch` heartbeat stops (Alertmanager →
    Healthchecks.io every 60 s; `deadmansswitch.md`);
  - `node-healthcheck.timer` (5 min, `tasks/13-healthcheck.yml`);
  - `stellarindex-smoke.timer` (5 min) and `stellarindex-sla-probe.timer`
    (15 min).
  Each of those channels is silently disabled if its URL in
  `/etc/default/{node-healthcheck,stellarindex-healthchecks}` is
  empty — if none of them fired for a real outage, check that first
  afterwards.

## Quick diagnosis (≤ 5 min)

```sh
# Can we reach the host at all? (public IP is in
# configs/ansible/inventory/r1.yml — gitignored)
ping -c 3 <r1-ip>
ssh -o ConnectTimeout=10 root@r1 uptime
# fail2ban is active on sshd (tasks/12-hardening.yml): don't hammer
# a failing login from one IP or you lock yourself out.

# From inside (if you can SSH):
systemctl status prometheus-node-exporter
journalctl -u prometheus-node-exporter -n 50
curl -s localhost:9100/metrics | head -1
ss -ltnp | grep :9100          # expect exactly ONE listener

# Do NOT act on `systemctl status node_exporter` — that is the retired
# pre-#33 hand-rolled unit. It is deliberately left on disk stopped +
# disabled and shows `inactive (dead)` on a healthy host
# (configs/ansible/roles/archival-node/tasks/10-observability.yml).

# If SSH fails: Hetzner Robot (https://robot.hetzner.com) → server r1
# (FSN1) → KVM console / Rescue system / Reset. There is no customer
# IPMI on Hetzner dedicated servers.
# TODO(ash): record the Robot server number and who holds Robot login
# here — nothing in the repo documents it.
```

## Typical root causes

1. **`prometheus-node-exporter` died but host is fine.** Exporter unit
   crashed or was OOM-killed; everything else keeps humming. Check
   with ssh + systemctl.

2. **Port-:9100 collision.** Someone started the legacy
   `node_exporter.service` alongside `prometheus-node-exporter`; the
   two binaries fight over :9100 (`10-observability.yml`, #33 cutover).
   `ss -ltnp | grep :9100` shows the wrong binary or a bind failure in
   the journal.

3. **Full-host reboot** — planned or unplanned. Check uptime; if
   `uptime` says < 5 min, that's the answer. (This one you'll find
   from the Healthchecks.io side, not from this alert.)

4. **Network-level isolation** — Hetzner switch/uplink failure, NIC
   gone. The host is alive but we can't talk to it. From the Robot
   KVM console you should be able to ping the default gateway.

5. **Hardware failure** — PSU, NVMe, mainboard, DIMM. The Robot KVM
   console will usually show something (POST errors, failed drive).
   Cross-check `nvme-smart.md` / `zfs-degraded.md` once the box is
   back.

6. **Kernel panic / hang.** KVM console shows the panic; SSH fails;
   pings fail. Hardware reset via Robot is the only path back.

## Mitigation

- [ ] Step 1 — decide if it's the exporter or the host (above). If
      `stellarindex_host_down` fired, it's the exporter/scrape path.
- [ ] Step 2 — if just the exporter: `systemctl restart
      prometheus-node-exporter`, then `curl -s localhost:9100/metrics |
      head -1` and wait one scrape (15 s) for `up` to return to 1.
      Never `systemctl start node_exporter` — that is the documented
      rollback path ONLY after `systemctl stop prometheus-node-exporter`.
- [ ] Step 3 — if the host is down: **r1 is single-host.** There is no
      failover. Host down = API, indexer, aggregator, galexie,
      ClickHouse, Postgres, Redis, MinIO, Prometheus, Alertmanager all
      down, and the alert pipeline itself is dark. Treat as SEV-1
      manually: post in Discord #stellarindex-pages and update the
      status page (`sev-status-page-update.md`). The only fix is
      getting the box back (Robot Reset → KVM if it doesn't come up).
- [ ] Step 4 — once the box is back, the fire moves to the service
      runbooks: `api-down.md`, `all-ingestion-down.md`,
      `galexie-catchup-refused.md`, `ch-live-sink-drops.md`. There is
      no stellar-core validator on r1 (`run_stellar_core: false`);
      galexie's captive core is the only stellar-core on the box.
- [ ] Step 5 — there is no HAProxy pool, Patroni cluster or Redis
      Sentinel on r1 to drain/promote. Caddy on :80/:443 proxies to
      the loopback API on :3000 (`tasks/19-caddy.yml`); it comes back
      with the host.
- [ ] Verification: `up{job="node_exporter"}` returns to 1 and
      `stellarindex_prometheus_scrape_failing` clears; for a real
      outage, all Healthchecks.io checks green again.

## After the host returns

- [ ] `zpool status -x` reports the `data` pool ONLINE (else
      `zfs-degraded.md`); `zfs list -o name,mountpoint,mounted` shows
      every `data/*` dataset mounted.
- [ ] `systemctl --failed` is empty.
- [ ] `systemctl is-active clickhouse-server postgresql@15-main
      redis-server galexie minio caddy cap67-movements
      stellarindex-indexer stellarindex-aggregator stellarindex-api
      prometheus-node-exporter prometheus` and `ss -ltnp | grep :9093`
      shows Alertmanager listening.
- [ ] galexie needs ~9 min of cold captive-core catchup before the
      indexer resumes — **do not restart it repeatedly**
      (`galexie-catchup-refused.md`). Watch `ingestion-lag.md` /
      `source-stopped.md` alerts clear on their own.
- [ ] Textfile-collector-driven alerts (`sla-probe-stale.md`,
      `supply-snapshot-stale.md`, `archive-completeness-stale.md`,
      `binary-version-skew.md`, `stellar-stack-version-lag.md`) may
      fire until the writing timers' first post-boot run
      (`OnBootSec`: heartbeat 30 s, smoke 2 min, node-healthcheck
      2 min, sla-probe 3 min; the daily `OnCalendar` ones run at
      their scheduled time). Don't chase them for the first ~15 min.
- [ ] `up{job="node_exporter"} == 1`, deadmansswitch heartbeat
      resumed, Healthchecks.io all green.

## Root cause analysis

- Hetzner Robot KVM console / Robot status-page for the incident
  window; Hetzner network-status for FSN1.
- `journalctl -b -1` (previous boot) once the box is back — panic,
  OOM, watchdog?
- `smartctl` / `nvme smart-log` on all four NVMes — was it a
  disk-failure cascade? (`nvme-smart.md`)
- If it was the exporter only: `journalctl -u prometheus-node-exporter`
  and `dmesg | grep -i oom`.

## Known false-positive patterns

- **node_exporter OOMed on a starved box.** If the host is thrashing
  (see `host-memory-high.md`), the exporter is often the first to
  get killed. The alert is technically accurate ("we lost visibility")
  but the underlying problem is memory, not host liveness.
- **Prometheus itself is sick.** r1 has one node_exporter instance,
  so "multiple instances down at once" carries no signal. Instead: if
  `up == 0` for `node_exporter` AND the other localhost jobs
  (`stellarindex-api` / `stellarindex-indexer` /
  `stellarindex-aggregator` / `caddy` / `galexie`) simultaneously,
  suspect Prometheus — restart, `/` full
  (`node-root-disk-full.md`), or TSDB corruption
  (`prometheus-tsdb-corruption.md`) — not the exporter.
- **Legacy `node_exporter` unit looks dead.** It is supposed to be.
  See Quick diagnosis.

## Related

- `host-cpu-high.md` / `host-memory-high.md` — resource-side
  precursors to a full host down.
- `scrape-failing.md` — `stellarindex_prometheus_scrape_failing`
  fires together with this alert for the same target.
- `exporter-down.md` — the P1 `stellarindex_{redis,postgres,pgbackrest,minio}_exporter_down`
  siblings.
- `deadmansswitch.md` — the out-of-band channel that actually
  detects r1 being dark.
- `prometheus-tsdb-corruption.md`, `zfs-degraded.md`,
  `nvme-smart.md`, `sev-status-page-update.md`.
- `bootstrap-archival-node.md` §"Firewall locked us out" — the
  Robot-KVM recovery path when nftables has shut you out.

## Changelog

- 2026-04-23 — initial draft.
- 2026-08-28 — re-verified against HEAD: job label `node_exporter`
  (F-1329), unit `prometheus-node-exporter` (#33 cutover), rule file
  `configs/prometheus/rules.r1/infra.yml`, single-host r1 topology
  (no HAProxy/Patroni/Sentinel/validator), Hetzner Robot instead of
  IPMI, Prometheus-on-r1 caveat + Healthchecks.io detectors, added
  post-return checklist. Ratified.
