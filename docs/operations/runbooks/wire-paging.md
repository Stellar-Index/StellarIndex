---
title: Wire paging (operator, ~20 min) — the last thing between alerts and a human
last_verified: 2026-07-27
status: active
severity: launch-blocking
---

# Wire paging

## At a glance

| | |
|---|---|
| **Symptom** | Alerts fire in the Alertmanager UI but nobody is notified. `pre-launch-check.sh` reports 4 FAILs on `HEALTHCHECKS_URL_*`. |
| **Impact** | The first-24h launch watch is blind. A dead pipeline looks identical to a healthy one. |
| **Who** | Operator only — needs accounts + secrets deliberately absent from the repo. |
| **Time** | ~20 min, fully reversible. |
| **Done when** | `pre-launch-check.sh` reports 0 failures AND `chat-page` shows a real webhook block, not a bare stub. |

**Today alerts route to nobody.** Alertmanager evaluates rules correctly
and accumulates firing alerts in its UI, but every fanout receiver is a
no-op stub because no webhook URLs are set. The first-24h launch watch
would be blind. This is the v1 launch plan's §1 "Launch mechanics" gate
and §3 register item 2b.

Everything below is operator-only: it needs accounts and secrets that
are deliberately not in the repo. All of it is reversible.

## What you are creating

| Thing | Count | Purpose |
|---|---|---|
| Healthchecks.io check — per binary | 3 | indexer / aggregator / api heartbeats. A MISSING ping is the alarm. |
| Healthchecks.io check — smoke | 1 | the 13-GET API smoke, every 5 min |
| Healthchecks.io check — SLA probe | 1 | per-run latency/freshness probe |
| Healthchecks.io check — deadmansswitch | 1 | Alertmanager itself pings this. Its silence means the alerting pipeline died. |
| Chat webhook | 1–2 | where `severity=page` and `severity=ticket` land |

Suggested Healthchecks.io schedules (dashboard side): binaries 60 s /
grace 120 s; smoke 5 min / grace 10 min; SLA probe per-run / grace 1 h.

## Step 1 — per-binary heartbeats

Create the 5 non-deadmansswitch checks above, then on r1:

```sh
sudo vi /etc/default/stellarindex-healthchecks
```

Fill these (they are already present, empty):

```
HEALTHCHECKS_URL_INDEXER=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_AGGREGATOR=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_API=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_SMOKE=https://hc-ping.com/<uuid>
HEALTHCHECKS_URL_SLA_PROBE=https://hc-ping.com/<uuid>
```

No restart needed — the timers source this file per run. An empty URL
skips the ping silently, which is why this failed quietly until now.

## Step 2 — Alertmanager fanout

⚠️ **Use these EXACT variable names.** Until 2026-07-27 this file
offered `SLACK_WEBHOOK_URL`, which nothing reads — filling that in and
applying produced silent no-op stubs. Corrected, but if you are on an
older box, check the names first.

```sh
sudo vi /etc/default/alertmanager-secrets
```

```
HEALTHCHECKS_DEADMANSSWITCH_URL=https://hc-ping.com/<uuid>
DISCORD_WEBHOOK_URL_PAGES=https://discord.com/api/webhooks/<id>/<token>
DISCORD_WEBHOOK_URL_ALERTS=https://discord.com/api/webhooks/<id>/<token>
```

Point both Discord URLs at the same webhook if you only want one
channel. Then apply — this validates with `amtool`, atomic-installs,
and reloads:

```sh
bash configs/alertmanager/apply.sh
```

The script reads the env file itself; you do not need to export
anything. An empty value still yields a no-op stub, so a typo'd
variable name fails silently — which is the trap this runbook exists
to close.

## Step 3 — verify (do not skip)

```sh
# From the repo root. Read-only; exit code = number of FAILs.
ssh root@136.243.90.96 'bash -s' < scripts/ops/pre-launch-check.sh
```

Before wiring, this reports **4 FAILs** (the four `HEALTHCHECKS_URL_*`)
and warns on the deadmansswitch and both Discord URLs. After wiring it
should report **0 failures**. That transition is the acceptance test.

Then confirm fanout actually reaches you, rather than trusting config:

```sh
# Does the receiver now carry a real webhook, not a bare stub?
ssh root@136.243.90.96 'grep -A3 "name: chat-page" /etc/prometheus/alertmanager.yml'
```

A wired receiver shows a `discord_configs:`/`webhook_configs:` block
beneath it. A bare `- name: chat-page` with nothing under it means the
substitution found an empty value.

## Step 4 — codify

Put the same values into the Ansible vault so a rebuild does not
silently land unwired:

```sh
cd configs/ansible && ansible-vault edit inventory/r1.secrets.yml
```

## Why the deadmansswitch matters most

Every other check alarms when something breaks. The deadmansswitch
alarms when **the alerting pipeline itself** breaks — Alertmanager
pings it continuously, and Healthchecks.io alerts you when those pings
stop. Without it, a dead Alertmanager is indistinguishable from a
perfectly healthy system. It is one URL and it is the one that catches
the failure mode nothing else can see.

## Related

- [alertmanager-bad-config.md](alertmanager-bad-config.md) — when
  `apply.sh` validation fails or Alertmanager refuses to reload.
- [../sev-playbook.md](../sev-playbook.md) — what to do once a page
  actually reaches you.
- [../v1-launch-plan.md](../v1-launch-plan.md) — §1 "Launch mechanics"
  gate and §3 register item 2b, which this runbook closes.
- `configs/alertmanager/README.md` — receiver/severity routing design.
