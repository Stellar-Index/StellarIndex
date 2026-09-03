---
title: Runbook — alertmanager-not-notifying
last_verified: 2026-09-03
status: current
severity: P1
---

# Runbook — `stellarindex_alertmanager_not_notifying`

> **This alert exists because of a real 31-day outage.** Between
> 2026-07-29 06:24 and 2026-08-29 22:14 Alertmanager delivered **zero**
> notifications through any integration. Page, ticket, default and the
> deadman's switch were all black holes. Nothing detected it, because
> the thing that reports problems was the problem.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_alertmanager_not_notifying` |
| Severity | page |
| Fires when | zero webhook notifications in 15 min, sustained 10 min |
| Means | the fan-out is dead — not that the estate is quiet |

## Why zero is always wrong

`stellarindex_deadmansswitch` fires permanently (`expr: vector(1)`) and
its route uses `repeat_interval: 1m`. So the `webhook` integration must
produce roughly **30 notifications an hour, forever**. The measured
steady-state rate on r1 is `30.0/hour`. Zero is never a quiet estate;
it means notifications are not leaving the process.

**While this alert is firing, it cannot reach you.** That is inherent.
Treat the Prometheus UI (`http://localhost:9090/alerts`) as the source
of truth, not Discord. The external Healthchecks.io check on the
deadman's switch is the out-of-band path that should have paged you
first — if it did not, see §4.

## 1. Confirm, and find out which leg

```sh
ssh root@136.243.90.96
curl -s localhost:9093/api/v2/status | python3 -c 'import sys,json;print(json.load(sys.stdin)["config"]["original"])' | grep -nE '_configs:|- name:'
curl -s 'localhost:9090/api/v1/query?query=alertmanager_notifications_total' \
  | python3 -c 'import sys,json;[print(r["metric"].get("integration"), r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'
```

Read the receiver list carefully. A receiver that shows `- name: X`
with **no following `*_configs:` block** accepts alerts and delivers
them to nobody. That is the failure mode. Only `silent` is supposed to
look like that.

## 2. The overwhelmingly likely cause: an empty URL

```sh
# names and lengths only — never print the values
awk -F= '/^(HEALTHCHECKS|DISCORD)/{printf "%s len=%d\n", $1, length($2)}' /etc/default/alertmanager-secrets
```

Any `len=0` is the bug. Alertmanager does not error on an empty URL —
`configs/alertmanager/apply.sh`'s renderer drops the whole `*_configs`
block so the file stays valid, and the reload then *succeeds*, which is
why `stellarindex_alertmanager_config_bad` stays silent.

## 3. Fix

Repopulate the missing URL(s) in `/etc/default/alertmanager-secrets`,
then re-apply from a checkout:

```sh
bash configs/alertmanager/apply.sh
```

`apply.sh` now refuses to install a config whose receivers deliver to
nobody, probes each URL for a live 2xx before installing, and reads the
running config back afterwards to assert the delivery blocks are
actually present. If it exits non-zero, it is telling you the real
problem — do not work around it with `ALERTMANAGER_ALLOW_EMPTY` unless
a receiver is genuinely meant to be dark.

Confirm recovery:

```sh
curl -s 'localhost:9090/api/v1/query?query=rate(alertmanager_notifications_total\{integration="webhook"\}[5m])*3600'
```

Expect it to climb back toward ~30/hour within a few minutes.

## 4. If the deadman's switch did not page you either

Then the out-of-band path is also broken, and that is a second incident.
`HEALTHCHECKS_DEADMANSSWITCH_URL` empty disarms it in exactly the same
way — the watchdog is defeated by the failure it exists to catch. Check
that the Healthchecks.io check still exists, has a grace period, and has
an escalation channel attached. That configuration lives off-box and no
in-repo check can prove it.

## Related

- [alertmanager-down](alertmanager-down.md) — the process itself is gone.
- [alertmanager-notifications-failing](alertmanager-notifications-failing.md) — delivery attempted and refused.
- [deadmansswitch](deadmansswitch.md) — the external last resort.
- `configs/alertmanager/README.md` — the secrets file and the apply flow.
