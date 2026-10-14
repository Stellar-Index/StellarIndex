---
title: Wire paging (operator, ~20 min) — the last thing between alerts and a human
last_verified: 2026-07-27
status: active
severity: launch-blocking
---

# Wire paging

Symptom: alerts fire in the Alertmanager UI but nobody is notified; `pre-launch-check.sh` reports 5 FAILs on `HEALTHCHECKS_URL_*`. Every fanout receiver is a no-op stub until webhook URLs are set, so the launch watch is blind (v1 launch plan paging gate, INV-0841). Operator only (accounts and secrets are not in the repo); ~20 min, reversible. Done when `pre-launch-check.sh` reports 0 failures AND `chat-page` shows a real webhook block.

## At a glance

Launch-blocking, operator only, ~20 min, reversible. Done when `pre-launch-check.sh` reports 0 failures.

## Create

- Healthchecks.io checks: indexer, aggregator, api (60 s / grace 120 s); smoke (5 min / grace 10 min); SLA probe (per-run / grace 1 h); deadmansswitch (Alertmanager pings it; its silence means the alerting pipeline died).
- 1-2 chat webhooks for `severity=page` and `severity=ticket`.

## Step 1 — per-binary heartbeats

```sh
sudo vi /etc/default/stellarindex-healthchecks
```

```
HEALTHCHECKS_URL_INDEXER=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_AGGREGATOR=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_API=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_SMOKE=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_SLA_PROBE=https://hc-ping.com/<uuid>
```

No restart needed (timers source the file per run). An empty URL skips the ping silently.

## Step 2 — Alertmanager fanout

Use these EXACT names (`SLACK_WEBHOOK_URL` is read by nothing; a typo'd name yields a silent no-op stub):

```sh
sudo vi /etc/default/alertmanager-secrets
```

```
HEALTHCHECKS_DEADMANSSWITCH_URL=https://hc-ping.com/<uuid>
DISCORD_WEBHOOK_URL_PAGES=https://discord.com/api/webhooks/<id>/<token>
DISCORD_WEBHOOK_URL_ALERTS=https://discord.com/api/webhooks/<id>/<token>
```

Both Discord URLs may be the same webhook. Apply (amtool validation, atomic install, reload):

```sh
bash configs/alertmanager/apply.sh
```

## Step 3 — verify (do not skip)

```sh
# From the repo root. Read-only; exit code = number of FAILs.
ssh root@136.243.90.96 'bash -s' < scripts/ops/pre-launch-check.sh

# Does the receiver now carry a real webhook, not a bare stub?
ssh root@136.243.90.96 'grep -A3 "name: chat-page" /etc/prometheus/alertmanager.yml'
```

Before wiring: 5 FAILs; after: 0. A wired receiver shows a `discord_configs:`/`webhook_configs:` block; a bare `- name: chat-page` means an empty value was substituted.

## Step 4 — codify

```sh
cd configs/ansible && ansible-vault edit inventory/r1.secrets.yml
```

Put the same values in the vault so a rebuild does not land unwired.

## Related

- [meta.md#stellarindex_alertmanager_config_bad](meta.md#stellarindex_alertmanager_config_bad), [../sev-playbook.md](../sev-playbook.md), [../v1-launch-plan.md](../v1-launch-plan.md), `configs/alertmanager/README.md`
