---
title: Runbook — alertmanager-notifications-failing
last_verified: 2026-09-03
status: current
severity: P2
---

# Runbook — `stellarindex_alertmanager_notifications_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_alertmanager_notifications_failing` |
| Severity | ticket |
| Fires when | `alertmanager_notifications_failed_total` increases over 15 min, sustained 5 min |
| Means | the pipeline is alive and the far end is rejecting it |

This is the *healthy-failure* case: Alertmanager is trying. Contrast
with [alertmanager-not-notifying](alertmanager-not-notifying.md), where
it is not trying at all. Steady state on r1 is **0 failures across
every integration and every reason**, so any sustained increase is real.

## 1. Identify the integration and the reason

```sh
ssh root@136.243.90.96
curl -s 'localhost:9090/api/v1/query?query=alertmanager_notifications_failed_total' \
  | python3 -c 'import sys,json;[print(r["metric"].get("integration"), r["metric"].get("reason"), r["value"][1]) for r in json.load(sys.stdin)["data"]["result"] if float(r["value"][1])>0]'
journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -i 'notify\|error'
```

| `reason` | Meaning | Usual cause |
| --- | --- | --- |
| `clientError` (4xx) | the far end rejected us | revoked Discord webhook, deleted Healthchecks check, wrong UUID |
| `serverError` (5xx) | the far end is broken | Discord or Healthchecks outage — usually self-clearing |
| `contextCanceled` / timeout | we gave up | host network, DNS, or egress firewall |

A `clientError` on the `webhook` integration is worth taking seriously:
Healthchecks.io returns 404 for an unknown UUID, so a non-zero
`clientError` there means **the deadman's switch is pinging a check
that no longer exists** — the alarm of last resort is disarmed while
looking healthy from our side.

## 2. Fix

For a revoked or mistyped credential, repopulate
`/etc/default/alertmanager-secrets` and re-apply:

```sh
bash configs/alertmanager/apply.sh
```

The apply now probes every configured URL for a live 2xx before
installing, so it will refuse a credential that has been revoked rather
than installing it and failing quietly at 3am.

For a far-end outage, confirm it is theirs (Discord status page) and
let it clear; the counter stops climbing on its own.

## 3. Verify

```sh
curl -s 'localhost:9090/api/v1/query?query=increase(alertmanager_notifications_failed_total[15m])' \
  | python3 -c 'import sys,json;print(sum(float(r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]))'
```

Expect `0.0`.

## Related

- [alertmanager-not-notifying](alertmanager-not-notifying.md)
- [alertmanager-down](alertmanager-down.md)
- [deadmansswitch](deadmansswitch.md)
