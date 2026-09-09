# R1 single-host Alertmanager config

Companion to [`configs/prometheus/prometheus.r1.yml`](../prometheus/prometheus.r1.yml)
+ [`configs/prometheus/rules.r1/`](../prometheus/rules.r1/).

Two parallel apply paths produce the same routing — and that is
enforced, not asserted. `scripts/ci/check-alertmanager-parity.sh`
renders both with identical inputs and fails on any difference in
`global`, `route`, `inhibit_rules` or `receivers`, on the wired and the
all-URLs-empty branch. It exists because the claim stood here as prose
for four months and was false for one of them (#501): the fix for #485
gave `informational` a real Discord receiver in this file only, leaving
the template one apply away from restoring the bug. **Change one file,
change the other, in the same commit.**

- **Standalone** (this directory): `apply.sh` env-substitutes the
  YAML and reloads systemd-managed Alertmanager. Use for one-off
  config changes without an Ansible run.
- **Ansible** (recommended for new deployments + multi-region):
  [`configs/ansible/roles/prometheus/templates/alertmanager.yml.j2`](../ansible/roles/prometheus/templates/alertmanager.yml.j2)
  renders the same shape via the
  [monitoring playbook](../ansible/playbooks/monitoring.yml).

Both paths use the `page` / `ticket` / `informational` severity
vocabulary defined in the severity ladder in
[docs/operations/sev-playbook.md](../../docs/operations/sev-playbook.md)
— matching every rule in `deploy/monitoring/rules/` +
`configs/prometheus/rules.r1/`.

## Routing

| Severity | Receiver | Cadence |
|----------|----------|---------|
| `page` | `chat-page` (Discord `#stellarindex-pages`) | every 12 h while firing |
| `ticket` | `chat-default` (Discord `#stellarindex-alerts`) | every 24 h while firing |
| `informational` | `chat-informational` (Discord `#stellarindex-informational`) | every 12 h while firing (the tree default); `send_resolved: false` |
| `stellarindex_deadmansswitch` | `deadmansswitch` (Healthchecks.io) | every 60 s |
| `stellarindex_alertmanager_notifications_failing` | `alert-delivery-failure` (Healthchecks.io) **and** the severity route | every 1 h |

`informational` stopped meaning "delivered to nobody" on 2026-09-08.
It gets its own low-traffic channel, deliberately separate from
`alerts` so a routine notice cannot bury a ticket in the same feed —
the routine notice that motivated it being an oracle publishing a
ticker we do not map yet. `DISCORD_WEBHOOK_URL_INFORMATIONAL` is
OPTIONAL: unset, the renderer strips the block and the receiver
degrades to the `silent` stub it replaced, so a host that has not
configured it is unchanged rather than broken. Nothing routes to
`silent` any more; the receiver is kept as that fallback shape.

The deadmansswitch is the alarm-of-last-resort — when its 60 s
heartbeat stops, Healthchecks.io pages us via a fully separate
channel, catching outages of Prometheus or Alertmanager itself.

`alert-delivery-failure` closes a different gap. The alert that
reports a refused delivery is a `ticket`, so by severity alone it is
delivered through the integration whose failure it is reporting — on
2026-09-07 the Discord receiver returned HTTP 400 for 11 hours and
that alert fired into the same dead channel the entire time. Its
route carries `continue: true`, so the alert reaches the out-of-band
check *in addition to* chat, never instead of it. The check is
deliberately separate from the deadmansswitch one: overloading that
signal would make "Prometheus is dead" and "chat delivery is refused"
indistinguishable.

## Message size is bounded on purpose

A Discord embed is rejected whole (HTTP 400, which Alertmanager treats
as **unrecoverable** — one attempt, no retry) if its description
exceeds 4096 characters, its title exceeds 256, or the two together
exceed 6000. Alertmanager 0.26 does not truncate either field.

So both Discord templates render at most **3** alerts per notification,
state the remainder ("… and N more in this group"), and cap every
interpolated field with `printf "%.Ns"` — summary 160, description 300,
runbook URL 200, severity 16, alertname 120, external URL 120. The
bound is hard: no annotation, label or URL can grow the payload. The
measured worst case is **2257 characters** at any group size (55 % of
the limit); the real 22-alert `stellarindex_oracle_stale` group that
caused the outage renders at **1378**, down from **9319**.

`apply.sh --check-only` and `amtool check-config` validate *syntax*,
not size. If you edit a template, re-render it against a large group
and count the characters.

## Apply to R1

1. **Provision the secrets file** off-disk in git
   (`/etc/default/alertmanager-secrets` on R1):

   ```sh
   # /etc/default/alertmanager-secrets — chmod 0600, root:root
   HEALTHCHECKS_DEADMANSSWITCH_URL='https://hc-ping.com/<your-uuid>'
   # Discord incoming webhooks (Server Settings → Integrations →
   # Webhooks → New Webhook → Copy URL). One per channel; point both
   # at the same URL if you only want a single channel.
   DISCORD_WEBHOOK_URL_PAGES='https://discord.com/api/webhooks/<id>/<token>'
   DISCORD_WEBHOOK_URL_ALERTS='https://discord.com/api/webhooks/<id>/<token>'
   # Optional, strongly recommended: a SECOND Healthchecks check (not
   # the deadmansswitch one) that carries delivery failures out of
   # band. Unset is allowed so provisioning never blocks an urgent
   # apply, but apply.sh prints "DARK" on every run until it is set.
   HEALTHCHECKS_ALERT_DELIVERY_URL='https://hc-ping.com/<a-different-uuid>'
   # Optional: the low-traffic channel every `informational` alert
   # goes to (2026-09-08). Unset leaves that receiver a stub, which is
   # the pre-2026-09-08 behaviour — a no-op, not a config error.
   DISCORD_WEBHOOK_URL_INFORMATIONAL='https://discord.com/api/webhooks/<id>/<token>'
   ```

   **The first three may not be empty.** An empty URL makes the
   renderer drop that receiver's config block, and the receiver then
   accepts alerts and delivers them to nobody — identical to `silent`,
   with a *successful* reload and every self-check green. That is
   precisely what happened between 2026-07-29 and 2026-08-29: all
   three URLs were unset, every path including the deadman's switch
   was a black hole, and nothing reported it for 31 days. The last two
   are optional by design (`AM_OPTIONAL_RECEIVERS` in `apply.sh`):
   their absence is reported on every run, not refused, so a
   provisioning gap can never block the apply that restores a broken
   chat channel.

   `apply.sh` therefore refuses to install a config whose receivers
   deliver to nobody, probes each URL for a live 2xx first (a revoked
   webhook fails as silently as an empty one), and reads the running
   config back afterwards to confirm the delivery blocks are really
   there. Deliberately running a receiver dark is possible but has to
   be named: `ALERTMANAGER_ALLOW_EMPTY=pages`. Pointing both Discord
   URLs at the same webhook is the better answer.

   If the guard ever fails open, `configs/alertmanager/apply-test.sh`
   is the self-test that proves it still fails closed; it runs in
   `verify.sh` and in CI.

2. **Run apply.sh** as root on R1:

   ```sh
   sudo /path/to/configs/alertmanager/apply.sh
   ```

   The script env-substitutes the YAML, validates with
   `amtool check-config`, installs to
   `/etc/prometheus/alertmanager.yml` (where the systemd unit
   expects it), and reloads `prometheus-alertmanager`.

## Verify

```sh
# Confirm the config loaded.
curl -s localhost:9093/-/healthy

# Trigger a synthetic alert to verify the chat fanout.
amtool alert add \
  --alertmanager.url=http://localhost:9093 \
  alertname=TEST_ALERT severity=ticket

# 30 seconds later, expect a Discord message in #stellarindex-alerts.
# Resolve:
amtool alert add \
  --alertmanager.url=http://localhost:9093 \
  alertname=TEST_ALERT severity=ticket --end=$(date -u +%FT%TZ)
```

## Migrate to multi-host

When R2 / R3 land, the Ansible role at
`configs/ansible/roles/prometheus/templates/alertmanager.yml.j2`
takes over. That template **already uses** the same
`page/ticket/informational` vocabulary this R1 file does (F-1265,
2026-05-13 — the template converged on the same severity ladder
post-R1 standup, so the multi-host transition is a config-shape
swap, not a severity-vocabulary rewrite). Decommission this
directory when the role applies cleanly to R1.
