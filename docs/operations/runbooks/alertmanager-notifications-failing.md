---
title: Runbook — alertmanager-notifications-failing
last_verified: 2026-09-08
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
| `other` | the integration returned an untyped error | **this is what a Discord rejection looks like** — the notifier reports `unexpected status code NNN` rather than a classified 4xx, so `integration="discord"` never lands in `clientError`. Read the status code out of the journal, not the metric |

A `clientError` on the `webhook` integration is worth taking seriously:
Healthchecks.io returns 404 for an unknown UUID, so a non-zero
`clientError` there means **the deadman's switch is pinging a check
that no longer exists** — the alarm of last resort is disarmed while
looking healthy from our side.

## 2. If it is Discord and the status code is 400 — the payload

```sh
journalctl -u prometheus-alertmanager --since -1h --no-pager \
  | grep -c 'unexpected status code 400'
journalctl -u prometheus-alertmanager --since -1h --no-pager \
  | grep -o 'num_alerts=[0-9]*' | sort | uniq -c
```

A 400 with body `{"embeds": ["0"]}` means Discord rejected the embed as
invalid, and the invalidating field is almost always its size: an embed
description may not exceed 4096 characters, a title 256, and the two
together 6000. Alertmanager 0.26 does not truncate either field, and it
classifies a 400 as **unrecoverable** — one attempt, no retry — so the
receiver is dead for *every* alert routed to it, not just the group
that overflowed.

That is the 2026-09-07 shape. `reflector-dex` stalled, 22
`stellarindex_oracle_stale` alerts landed in a single
`group_by: [alertname, severity]` group, and the then-unbounded
`range .Alerts` template rendered a 9319-character description. The
`chat-default` receiver returned 400 every 5 minutes for 11 hours;
every other ticket alert in that window was dropped with it, including
this one.

Both Discord templates are now bounded — at most three alerts rendered,
a "… and N more in this group" line, and `printf "%.Ns"` on every
interpolated field — so a large group can no longer produce an invalid
payload. Measured worst case is 2257 characters at any group size. If
you see a fresh 400 anyway, re-measure before assuming the far end
changed: render the receiver's `message` template against the real
group and count.

To measure a live group on r1 (read-only; `amtool template render` uses
Alertmanager's own template engine, so the count is the real one):

```sh
curl -s localhost:9093/api/v2/alerts > /tmp/alerts.json

# Reshape the API's alert list into amtool's --template.data format.
python3 - <<'PY' > /tmp/tdata.json
import json
a = [x for x in json.load(open('/tmp/alerts.json'))
     if x['labels']['alertname'] == 'stellarindex_oracle_stale']   # the group
json.dump({"receiver": "chat-default", "status": "firing",
           "alerts": [{"status": x["status"]["state"], "labels": x["labels"],
                       "annotations": x["annotations"], "startsAt": x["startsAt"],
                       "endsAt": x["endsAt"], "generatorURL": x["generatorURL"],
                       "fingerprint": x["fingerprint"]} for x in a],
           "groupLabels": {k: a[0]["labels"][k] for k in ("alertname", "severity")},
           "commonLabels": {}, "commonAnnotations": {},
           "externalURL": "http://localhost:9093"}, open('/tmp/tdata.json', 'w'))
PY

: > /tmp/empty.tmpl
MSG=$(python3 -c "import yaml;d=yaml.safe_load(open('/etc/prometheus/alertmanager.yml'));\
print([c['message'] for r in d['receivers'] if r['name']=='chat-default' \
       for c in r['discord_configs']][0],end='')")
amtool template render --template.glob=/tmp/empty.tmpl \
  --template.text="$MSG" --template.data=/tmp/tdata.json | wc -m
```

Characters, not bytes — `wc -m` under a UTF-8 locale is the right count;
Discord's limit is in characters and the templates emit em dashes and
ellipses. Anything approaching 4096 is a defect in the template, not a
big incident.

## 3. Fix

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

## 4. Verify

`-G --data-urlencode`, not a bare query string: Prometheus rejects the
unencoded `[` of a range selector and answers with an EMPTY body, which
this pipeline then reports as a JSON decode error rather than as a
number. (The earlier inline form did exactly that on every run.)

```sh
curl -s -G localhost:9090/api/v1/query \
  --data-urlencode 'query=increase(alertmanager_notifications_failed_total[15m])' \
  | python3 -c 'import sys,json;print(sum(float(r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]))'
```

Expect `0.0`.

## Why this alert can be the one you never see

Its severity is `ticket`, so by severity alone it routes to
`chat-default` — the Discord receiver. When Discord is the integration
that is failing, this alert is delivered through the path whose failure
it is reporting, and nobody sees it. That is what happened for the whole
11-hour window above.

The routing tree therefore also sends it to `alert-delivery-failure`,
a Healthchecks.io webhook on a **separate** check from the deadman's
switch, with `continue: true` so chat still gets it as well. Provision
the check to arm that path:

1. Create a new check in Healthchecks.io — name it for delivery
   failures, not for the deadman's switch. Overloading the deadman
   check (by pinging its `/fail` endpoint) would make one signal mean
   both "Prometheus or Alertmanager is dead" and "chat delivery is
   being refused", which sends the operator to the wrong runbook.
   Give it a long period/grace: it is pinged only when something is
   wrong, so it must not go down on its own.
2. Add the ping URL to `/etc/default/alertmanager-secrets` on r1 as
   `HEALTHCHECKS_ALERT_DELIVERY_URL`.
3. `bash configs/alertmanager/apply.sh` — it prints
   `optional receiver 'delivery-failure' is wired` when the URL is set
   and `… is DARK` on every run until it is.

Until step 2 is done the receiver renders as a stub and this alert
reaches chat only, exactly as before — no worse, but no better.

## Related

- [alertmanager-not-notifying](alertmanager-not-notifying.md)
- [alertmanager-down](alertmanager-down.md)
- [deadmansswitch](deadmansswitch.md)
