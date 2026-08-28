---
title: Runbook — deadmansswitch
last_verified: 2026-08-28
status: current
severity: P1
---

# Runbook — `stellarindex_deadmansswitch`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_deadmansswitch` |
| Semantics | **Inverted** — this alert fires constantly by design; you page when it **stops** firing. |
| Severity | P1 when it stops (escalated via the external watchdog). Note the rule's own `severity` label is `informational` **by design** — the Alertmanager routing tree matches on `alertname`, not severity, to send it to the watchdog receiver, and an `informational` label keeps it out of the page/ticket fanout. |
| Detected by | `configs/prometheus/rules.r1/meta.yml` (group `stellarindex.meta`, `expr: vector(1)`, `for: 0s`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/meta.yml`. Routed by `configs/alertmanager/alertmanager.r1.yml` to the healthchecks.io receiver. |
| Typical MTTR | Whatever it takes to bring Prometheus or Alertmanager back up — minutes to an hour |
| Impact | If this stops firing, we've lost our primary alerting pipeline. Every other alert is invisible until it's restored — we could be in an outage and not know. |

## How it works

- `expr: vector(1)` — always truthy, always firing.
- Routed to an **external watchdog** (Healthchecks.io) via
  Alertmanager. That watchdog expects a heartbeat on every repeat
  interval; the ping target is an `https://hc-ping.com/<uuid>` check.
- If the watchdog stops receiving → **it pages us** on a separate
  channel (whatever notification integrations the Healthchecks.io
  check is configured with — independent of our Alertmanager).

**The "alert" you're looking at here (in Prometheus) is the
*positive* case.** It's firing means the pipeline is healthy. The
page we respond to is the watchdog's silence, not the alert itself.

## Where the watchdog URL lives (secrets convention)

`configs/alertmanager/alertmanager.r1.yml` contains only the
placeholder `${HEALTHCHECKS_DEADMANSSWITCH_URL}` — the real webhook
URLs are **never committed**. They live on r1 in
`/etc/default/alertmanager-secrets` (root:root, `0600`):

```sh
# /etc/default/alertmanager-secrets
HEALTHCHECKS_DEADMANSSWITCH_URL='https://hc-ping.com/<uuid>'
DISCORD_WEBHOOK_URL_PAGES='https://discord.com/api/webhooks/<id>/<token>'
DISCORD_WEBHOOK_URL_ALERTS='https://discord.com/api/webhooks/<id>/<token>'
```

`configs/alertmanager/apply.sh` sources that file, substitutes the
placeholders (an empty URL degrades the receiver to a no-op stub
rather than breaking the config), validates with `amtool`, and
atomic-installs to `/etc/prometheus/alertmanager.yml`. So: to rotate
or fix the watchdog URL, edit `/etc/default/alertmanager-secrets` on
r1 and re-run `configs/alertmanager/apply.sh` — never hand-edit the
rendered `/etc/prometheus/alertmanager.yml`. See
`configs/alertmanager/README.md`.

## Symptoms (when it fails)

- You got paged by the secondary channel: "deadmansswitch
  heartbeat missed".
- Prometheus/Alertmanager dashboards may be green or offline from
  your POV — both are possible.
- Primary oncall tool is silent (paradoxically reassuring — that's
  what we were testing).

## Quick diagnosis (≤ 5 min)

```sh
# Prometheus + Alertmanager both run on r1, apt-installed,
# listening on localhost — check them from the host:
ssh root@136.243.90.96 'curl -s localhost:9090/-/healthy; curl -s localhost:9093/-/healthy'

# From the watchdog's POV, when did it last hear from us?
# (Healthchecks.io dashboard — the hc-ping.com/<uuid> check.)

# Is the deadmansswitch route still configured in the RENDERED config?
ssh root@136.243.90.96 'amtool --alertmanager.url=http://localhost:9093 config routes show'
```

## Typical root causes

1. **Prometheus is down / unreachable.** Can't evaluate the
   `vector(1)` expression, can't fire the alert.

2. **Alertmanager is down / unreachable.** Can't route it to the
   watchdog.

3. **Network path to the watchdog broken.** Prometheus fires →
   Alertmanager routes → outbound HTTPS to `hc-ping.com` fails
   (DNS, proxy, TLS).

4. **The watchdog URL is empty or wrong in the rendered config.**
   `apply.sh` renders an empty `HEALTHCHECKS_DEADMANSSWITCH_URL`
   into a **no-op stub receiver** — valid config, zero pings. Check
   `/etc/default/alertmanager-secrets` and re-run `apply.sh`.

5. **Someone silenced the alert** in Alertmanager. Deadmansswitch
   should never be silenced; if it is, that's a config mistake.

6. **The `stellarindex.meta` rule group is disabled** (misconfig
   or rule-loading error). `alertmanager_config_last_reload_successful`
   / `prometheus_rule_group_iterations_total` will tell you.

## Mitigation

- [ ] Step 1 — find which component is down (above).
- [ ] Step 2 — restore it. Prometheus and Alertmanager are
      apt-installed systemd units on r1:
      ```sh
      ssh root@136.243.90.96
      systemctl status prometheus prometheus-alertmanager --no-pager
      systemctl restart prometheus              # if Prometheus is wedged
      systemctl restart prometheus-alertmanager # if Alertmanager is wedged
      ```
- [ ] Step 3 — confirm the watchdog starts receiving heartbeats
      again (watch the Healthchecks.io dashboard).
- [ ] Step 4 — do NOT ack the secondary page until you've
      verified the primary channel is functional end-to-end. Send
      a test alert through Alertmanager if in doubt.
- [ ] Verification: watchdog's "last ping" is recent and the check
      is back to "up"; other alerts can route through Alertmanager;
      a test alert fires and clears cleanly.

## Known false-positive patterns

- **Watchdog provider outage.** If Healthchecks.io is down, it
  can't hear us. Cross-check from an independent network (your
  phone) that the provider is up.
- **Network egress filter** — a firewall change that blocks
  outbound to `hc-ping.com` will silence us without anything being
  wrong on our side. Whitelist the hostname explicitly.

## Related

- `alertmanager-bad-config.md` — common root cause.
- `scrape-failing.md` — when Prometheus loses its targets.
- `configs/healthchecks/README.md` — the per-binary heartbeat timers
  (also Healthchecks.io) are the complementary signal: the
  deadmansswitch says "alerting pipeline alive", the per-binary
  checks say **which service died**.
- The Healthchecks.io status page (bookmark it).

## Changelog

- 2026-08-28 — re-verified against HEAD. Pod-restart advice replaced
  with the r1 systemd units (`prometheus` /
  `prometheus-alertmanager`); commands rehosted to
  `ssh root@136.243.90.96` + localhost ports. Added the secrets
  convention section that `alertmanager.r1.yml` points here for
  (`/etc/default/alertmanager-secrets`, injected by
  `configs/alertmanager/apply.sh`; empty URL ⇒ no-op stub). Rule
  citation → `rules.r1/meta.yml`; noted the deliberate
  `severity: informational` label (routing is by alertname). Related
  section gained `configs/healthchecks/README.md`.
- 2026-04-23 — initial draft. Emphasises the inverted semantics —
  this is the "test that our tests work" alert.
