# R1 single-host Alertmanager config

Companion to [`configs/prometheus/prometheus.r1.yml`](../prometheus/prometheus.r1.yml)
+ [`configs/prometheus/rules.r1/`](../prometheus/rules.r1/).

Two parallel apply paths produce the same routing:

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
| `informational` | `silent` (Alertmanager UI only) | — |
| `stellarindex_deadmansswitch` | `deadmansswitch` (Healthchecks.io) | every 60 s |

The deadmansswitch is the alarm-of-last-resort — when its 60 s
heartbeat stops, Healthchecks.io pages us via a fully separate
channel, catching outages of Prometheus or Alertmanager itself.

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
   ```

   **None of these may be empty.** An empty URL makes the renderer
   drop that receiver's config block, and the receiver then accepts
   alerts and delivers them to nobody — identical to `silent`, with a
   *successful* reload and every self-check green. That is precisely
   what happened between 2026-07-29 and 2026-08-29: all three URLs
   were unset, every path including the deadman's switch was a black
   hole, and nothing reported it for 31 days.

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
