---
title: Runbook — meta (alerting pipeline self-health)
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — meta alerts

Alerts that watch Prometheus, Alertmanager and the exporters (`configs/prometheus/rules.r1/meta.yml`). Page: `stellarindex_alertmanager_down`, `stellarindex_alertmanager_not_notifying`, the four `*_exporter_down`; the rest are tickets; `stellarindex_deadmansswitch` is informational with inverted semantics.

r1: Prometheus (`prometheus.service`, config `configs/prometheus/prometheus.r1.yml` installed to `/etc/prometheus/`) and Alertmanager (`prometheus-alertmanager.service`, NOT the inactive `alertmanager` unit) run on the one host; targets are `localhost:<port>`. Alertmanager source `configs/alertmanager/alertmanager.r1.yml`, live `/etc/prometheus/alertmanager.yml`, secrets `/etc/default/alertmanager-secrets` (`0600`): `HEALTHCHECKS_DEADMANSSWITCH_URL`, `DISCORD_WEBHOOK_URL_PAGES`, `DISCORD_WEBHOOK_URL_ALERTS`, optional `DISCORD_WEBHOOK_URL_INFORMATIONAL`, `HEALTHCHECKS_ALERT_DELIVERY_URL`. `bash configs/alertmanager/apply.sh` renders (empty optional URL gives a no-op stub; empty required URL is refused unless `ALERTMANAGER_ALLOW_EMPTY`), validates, installs, reloads. Never hand-edit the rendered file.

Shared commands (host `ssh root@136.243.90.96`):

```sh
curl -s localhost:9090/-/healthy; curl -s localhost:9093/-/healthy
journalctl -u prometheus-alertmanager -n 100 --no-pager
amtool check-config /etc/prometheus/alertmanager.yml
# delivery rate on the webhook integration; steady state ~30/hour
curl -s 'localhost:9090/api/v1/query?query=rate(alertmanager_notifications_total\{integration="webhook"\}[5m])*3600'
```

## At a glance

Alerting-pipeline self-health; page for Alertmanager down/not notifying and the exporter-down alerts, ticket for the rest. Use `http://localhost:9090/alerts` when Alertmanager itself is broken.

## stellarindex_prometheus_scrape_failing

**Symptom.** `up{job=~"stellarindex-api|stellarindex-indexer|stellarindex-aggregator|node_exporter|prometheus|caddy|galexie"} == 0` for 2 min (ticket). Visibility is lost; the service may be fine. The redis/postgres/pgbackrest/minio exporters have their own [page alerts](#stellarindex_redis_exporter_down).

**Check.**

```sh
# Prometheus's view of the failing target; lastError says why (refused, TLS, 404, parse error)
curl -s http://localhost:9090/api/v1/targets?state=active | \
  jq '.data.activeTargets[] | select(.health != "up") | {job: .labels.job, instance: .labels.instance, lastError: .lastError}'
curl -s http://localhost:<port>/metrics | head
systemctl list-units | grep -i exporter      # unit names vary by package
systemctl status <exporter-unit> --no-pager | head -15
```

**Likely causes.** Unit down (`for: 2m` absorbs a restart); static-config drift; rotated exporter credentials (401); the target service itself down (see its alerts).

**Fix.**

1. Exporter crash: `systemctl restart <exporter>`.
2. Drift: install `configs/prometheus/prometheus.r1.yml` to `/etc/prometheus/` and SIGHUP the unit.
3. Auth drift: rotate the vault entry and re-apply.
4. Verify `up` returns to 1.

**Escalate:** none (ticket).

## stellarindex_alertmanager_config_bad

**Symptom.** `alertmanager_config_last_reload_successful == 0` for 5 min (ticket). Changes since the last good load are NOT live; existing routes keep working, new routes go nowhere. Log shows `error loading config: ...`.

**Check.** `journalctl -u prometheus-alertmanager -n 100 --no-pager | grep -iE 'reload|error'` and `amtool check-config /etc/prometheus/alertmanager.yml`.

**Likely causes.** YAML typo; template reference errors (surface only at send time); version skew between the apt Alertmanager and the config. An unset optional secret is not a load failure: the receiver becomes a stub (see [not_notifying](#stellarindex_alertmanager_not_notifying)).

**Fix.**

1. Validate the checked-in source: `ALERTMANAGER_SECRETS=/dev/null bash configs/alertmanager/apply.sh --check-only`; fix `alertmanager.r1.yml`.
2. Confirm `/etc/default/alertmanager-secrets` sets the three required URLs.
3. Apply: `bash configs/alertmanager/apply.sh`.
4. Manual reload: `systemctl reload prometheus-alertmanager`. Do not `curl -XPOST http://localhost:9093/-/reload`: the unit runs without `--web.enable-lifecycle`.
5. Verify `alertmanager_config_last_reload_successful == 1`.

**Escalate:** none; a broken Alertmanager is caught by [deadmansswitch](#stellarindex_deadmansswitch) and [scrape_failing](#stellarindex_prometheus_scrape_failing).

## stellarindex_alertmanager_down

**Symptom.** `up{job="alertmanager"} == 0 or absent_over_time(up{job="alertmanager"}[10m]) == 1` for 5 min (page). Nothing can be delivered, including this: use the Prometheus UI (`http://localhost:9090/alerts`), not Discord.

**Check.**

```sh
systemctl status prometheus-alertmanager    # not `alertmanager`
journalctl -u prometheus-alertmanager -n 80 --no-pager
ss -lntp | grep 9093
```

**Likely causes.** Bad config installed by hand (restore `/etc/prometheus/alertmanager.yml` from the last good copy, then re-apply); OOM-killed (`stellarindex_host_memory_high`); port 9093 already bound (`systemctl stop`, `ss -lntp`, start); `job_name: alertmanager` missing from `/etc/prometheus/prometheus.yml`.

**Fix.**

1. `systemctl restart prometheus-alertmanager`, then `curl -s localhost:9093/api/v2/status | head -c 300`.
2. A running process is not proof of fan-out: check the webhook delivery rate; if zero, see [not_notifying](#stellarindex_alertmanager_not_notifying).

**Escalate:** page; SEV per [sev-playbook](../sev-playbook.md).

## stellarindex_alertmanager_not_notifying

**Symptom.** `sum(increase(alertmanager_notifications_total{job="alertmanager",integration="webhook"}[15m])) == 0` or absent, sustained 10 min (page). The deadman's switch sends ~30/hour; zero means notifications are not leaving the process. `sum()` without `by` is load-bearing (GH-1173). This alert cannot reach you: use `http://localhost:9090/alerts`.

**Check.**

```sh
curl -s localhost:9093/api/v2/status | python3 -c 'import sys,json;print(json.load(sys.stdin)["config"]["original"])' | grep -nE '_configs:|- name:'
curl -s 'localhost:9090/api/v1/query?query=alertmanager_notifications_total' \
  | python3 -c 'import sys,json;[print(r["metric"].get("integration"), r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'
# Names and lengths only, never values:
awk -F= '/^(HEALTHCHECKS|DISCORD)/{printf "%s len=%d\n", $1, length($2)}' /etc/default/alertmanager-secrets
```

A receiver with `- name: X` and no following `*_configs:` block delivers to nobody. Legitimate: `silent`, and `chat-informational` while `DISCORD_WEBHOOK_URL_INFORMATIONAL` is unset. Any `len=0` is the bug (the reload succeeds, so `config_bad` stays silent).

**Likely causes.** Empty URL in the secrets file (overwhelmingly); receivers rendered as stubs after a bad `apply.sh`.

**Fix.**

1. Repopulate the URL(s), then `bash configs/alertmanager/apply.sh`. It refuses receivers that deliver to nobody, probes each URL for a 2xx and reads the running config back. Do not use `ALERTMANAGER_ALLOW_EMPTY` unless a receiver is meant to be dark.
2. Confirm the webhook rate returns toward ~30/hour in a few minutes.
3. If the deadman's switch did not page you either, the out-of-band path is broken too: check the Healthchecks.io check exists with a grace period and an escalation channel (off-box, not verifiable in repo).

**Escalate:** page; SEV per [sev-playbook](../sev-playbook.md).

## stellarindex_alertmanager_notifications_failing

**Symptom.** `increase(alertmanager_notifications_failed_total{job="alertmanager"}[15m]) > 0` for 5 min (ticket). Alertmanager is trying and the far end rejects it (contrast [not_notifying](#stellarindex_alertmanager_not_notifying)). Steady state is 0.

**Check.**

```sh
curl -s 'localhost:9090/api/v1/query?query=alertmanager_notifications_failed_total' \
  | python3 -c 'import sys,json;[print(r["metric"].get("integration"), r["metric"].get("reason"), r["value"][1]) for r in json.load(sys.stdin)["data"]["result"] if float(r["value"][1])>0]'
journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -i 'notify\|error'
journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -c 'unexpected status code 400'
journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -o 'num_alerts=[0-9]*' | sort | uniq -c
```

**Likely causes.**

**Likely causes.** `clientError` (4xx): revoked Discord webhook, deleted Healthchecks check or wrong UUID (on the `webhook` integration the deadman's switch is disarmed: 404). `serverError` (5xx): provider outage. `contextCanceled`: network, DNS, egress. `other`: Discord rejections land here (`unexpected status code NNN` in the journal); a 400 `{"embeds": ["0"]}` is an oversize embed (description 4096, total 6000 characters) and is never retried. Both Discord templates are bounded (worst case 2257 characters).

**Fix.**

1. Credential: repopulate `/etc/default/alertmanager-secrets`, run `bash configs/alertmanager/apply.sh` (it probes every URL). Far-end outage: confirm on the provider status page and let it clear.
2. On a fresh 400, re-measure the rendered message (read-only):

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

   Near 4096 is a template defect, not a big incident.
3. Verify (use `-G --data-urlencode`; an unencoded `[` returns an empty body). Expect `0.0`:

   ```sh
   curl -s -G localhost:9090/api/v1/query \
     --data-urlencode 'query=increase(alertmanager_notifications_failed_total[15m])' \
     | python3 -c 'import sys,json;print(sum(float(r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]))'
   ```
4. Out-of-band path: create a separate Healthchecks.io check for delivery failures (period and grace 365 days, `filter_http_body`, failure keyword `"status":"firing"`, success keyword `"status":"resolved"`, notify by email), put its URL in `/etc/default/alertmanager-secrets` as `HEALTHCHECKS_ALERT_DELIVERY_URL`, run `bash configs/alertmanager/apply.sh`.

**Escalate:** none (ticket).

## stellarindex_alertmanager_optional_receiver_dark

**Symptom.** `stellarindex_alertmanager_optional_receiver_dark == 1` for 1 h (ticket). `apply.sh` installed an optional receiver with an empty URL and dropped its `*_configs` block: `delivery-failure` (`HEALTHCHECKS_ALERT_DELIVERY_URL`) or `informational` (`DISCORD_WEBHOOK_URL_INFORMATIONAL`); the `receiver` label says which. Whatever routes there is discarded. Nothing is being refused (contrast [notifications_failing](#stellarindex_alertmanager_notifications_failing)).

**Fix.** Set the URL in `/etc/default/alertmanager-secrets`, run `bash configs/alertmanager/apply.sh` (arming steps for `delivery-failure` are in notifications_failing step 4). An unset `informational` URL is a deliberate no-op if you want it dark.

**Escalate:** none.

## stellarindex_redis_exporter_down

**Symptom.** `up{job="redis_exporter"} == 0 OR absent_over_time(up{job="redis_exporter"}[5m]) == 1` for 2 min (page). Same shape for all four exporter alerts. An exporter outage blinds every dependent alert: the subsystem can be on fire and nobody is paged. Dependent alerts going silent (not OK, not evaluating) is the tell.

**Check** (all four).

```sh
systemctl status prometheus-redis-exporter      # redis
systemctl status prometheus-postgres-exporter   # postgres
systemctl status pgbackrest_exporter            # pgbackrest
systemctl status minio                          # minio (server itself)
journalctl -u <unit> -n 200 --no-pager          # crash / auth / config errors
curl -sf http://localhost:9121/metrics | head -5   # redis
curl -sf http://localhost:9187/metrics | head -5   # postgres
curl -sf http://localhost:9854/metrics | head -5   # pgbackrest
curl -sf -H "Authorization: Bearer $(cat /etc/prometheus/minio.token)" \
     http://localhost:9000/minio/v2/metrics/cluster | head -5   # minio
```

**Likely causes.** Exporter crashed; stale auth (postgres DSN drift, MinIO token file missing or rotated); permissions regression on the socket or data dir; the underlying service is down (follow its runbook).

**Fix.**

1. `systemctl restart <unit>`; read the journal if it will not start.
2. Verify the curl returns metrics, `up{job="<job>"}` is 1 (alert resolves in ~2 min) and the dependent family is back (redis: `redis_rdb_last_bgsave_status` present and `== 1`).
3. Capture RCA: `journalctl -u <unit> --since "30 min ago"`, `df -h`, `free -m`, `dmesg -T | tail -50`.

Redis: unit `prometheus-redis-exporter`, port 9121 (`/etc/default/prometheus-redis-exporter`), installed by `configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml`. Blinds `configs/prometheus/rules.r1/cache.yml` and `stellarindex_redis_writes_blocked` (`cache.md#stellarindex_redis_writes_blocked`).

**Escalate:** page; silence only for planned exporter upgrades.

## stellarindex_postgres_exporter_down

As [redis_exporter_down](#stellarindex_redis_exporter_down) with `job="postgres_exporter"`. Unit `prometheus-postgres-exporter`, port 9187 on `127.0.0.1`; `DATA_SOURCE_NAME` in `/etc/default/prometheus-postgres-exporter` (peer auth on the local socket as role `prometheus`, needs `pg_monitor`). Blinds every `pg_*` alert in `configs/prometheus/rules.r1/storage.yml` (`stellarindex_timescale_primary_down`, `_replica_lag`, `_lock_table_pressure`, `_connections_saturated`).

## stellarindex_pgbackrest_exporter_down

As [redis_exporter_down](#stellarindex_redis_exporter_down). The absent arm counts only if the exporter ever reported (`absent_over_time(up{job="pgbackrest_exporter"}[5m]) == 1 and on () count(stellarindex_pgbackrest_backup_last_rc) > 0`); `up == 0` fires unconditionally. An exporter that is up with no stanza is `stellarindex_pgbackrest_backup_metrics_absent`. Unit `pgbackrest_exporter`, port 9854 (the role fails fast unless `pgbackrest_exporter_release_sha256` is set in inventory). Blinds `stellarindex_timescale_backup_failed` and `stellarindex_timescale_backup_none_24h`.

## stellarindex_minio_exporter_down

As [redis_exporter_down](#stellarindex_redis_exporter_down) with `up{job="minio"}`; the scrape target is MinIO itself, unit `minio`, port 9000. Bearer token `/etc/prometheus/minio.token`: a missing file or 401 is the common cause (Prometheus must reload after rotation); regenerate per [credential-rotation.md](../credential-rotation.md#prometheus-bearer-token-regen-inv-0981inv-1144--now-codified). Blinds the galexie-archive, archive-completeness, archive-publish and object-storage families.

## stellarindex_deadmansswitch

**Symptom.** Inverted: it fires constantly by design (`expr: vector(1)`, severity `informational`); the Healthchecks.io check `https://hc-ping.com/<uuid>` pages you on a secondary channel when it STOPS. P1: every other alert is then invisible.

**Check.**

```sh
curl -s localhost:9090/-/healthy; curl -s localhost:9093/-/healthy
# Healthchecks.io dashboard: when did the hc-ping.com/<uuid> check last hear from us?
amtool --alertmanager.url=http://localhost:9093 config routes show   # route still in the RENDERED config?
```

**Likely causes.** Prometheus or Alertmanager down; network path to `hc-ping.com` broken; empty or wrong `HEALTHCHECKS_DEADMANSSWITCH_URL` (format `'https://hc-ping.com/<uuid>'`; an allowed-empty render is a no-op stub); silenced (never silence it) or the `stellarindex.meta` rule group failed to load (`alertmanager_config_last_reload_successful`, `prometheus_rule_group_iterations_total`).

**Fix.**

1. `systemctl status prometheus prometheus-alertmanager --no-pager`; restart the wedged one.
2. URL fix: edit the secrets file and re-run `bash configs/alertmanager/apply.sh`.
3. Do NOT ack the secondary page until a test alert routes end to end; watch for heartbeats to resume.
4. Healthchecks.io provider outage: cross-check from an independent network.

**Escalate:** the external watchdog escalates by itself.

## stellarindex_metrics_registry_absent

**Symptom.** `stellarindex_metrics_registry_present{component} == 0` (P3; `deploy/monitoring/rules/metrics-registry.yml`): a component runs without a Prometheus Registry, so alerts on its metrics can never fire. An absent series is not alerted.

**Likely causes.** `component="ledgerstream"` is the known accepted state, excluded from the rule: `pipeline.LedgerstreamConfig` leaves `Registry` nil because the SDK registration is not idempotent and repeated `ledgerstream.Stream` calls would panic. Only SDK buffer metrics are missing; the `both_missing` page is live. Any other component: a wiring regression.

**Fix.**

1. Read the `component` label on the firing series.
2. Other component: restore the Registry wiring in its constructor (code change and deploy).
3. Optional ledgerstream fix: make the SDK `WithMetrics` / `ApplyLedgerMetadata` registration idempotent, then wire `obs.Registry` through `pipeline.LedgerstreamConfig`.
4. Verify on the indexer's `/metrics`: `stellarindex_metrics_registry_present{component="..."} 1`.

**Escalate:** none (P3).

## stellarindex_healthcheck_ping_undelivered

**Symptom.** `increase(stellarindex_healthcheck_ping_failures_total[15m]) > 0 and (time() - stellarindex_healthcheck_ping_last_success_unix) > (15 * 60)`, `for: 5m`, ticket (`configs/prometheus/rules.r1/healthcheck-ping.yml`). `configs/healthchecks/hc-ping.sh` logs failed pings to the journal and `/var/lib/node_exporter/textfile_collector/hc_ping_<check>.prom`; a Healthchecks.io "down" notice is then about this host's egress, not the service. Wiring: `configs/ansible/roles/archival-node/tasks/17-stellarindex-healthchecks.yml`. Companions: [`sla-probe.md#stellarindex_api_smoke_failing`](sla-probe.md#stellarindex_api_smoke_failing), [`sla-probe.md#stellarindex_api_smoke_stale`](sla-probe.md#stellarindex_api_smoke_stale).

**Check.**

```sh
# curl's exit code names the layer: 6 = DNS, 7 = connect refused,
# 22 = HTTP error from hc-ping.com, 28 = timeout.
journalctl -u 'stellarindex-*' --since '1 hour ago' | grep hc-ping

# Is the service under the check actually healthy? Answer BEFORE acting on
# any Healthchecks.io notice for it.
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:3000/v1/healthz
systemctl list-timers 'stellarindex-*' --all

# Can this host reach the pinger at all? (No URL, so no secret.)
curl -fsS -o /dev/null -w '%{http_code}\n' https://hc-ping.com/
```

**Likely causes.** `rc=6` DNS; `rc=7` / `rc=28` egress blocked or hc-ping.com down (<https://status.healthchecks.io>); `rc=22` check deleted or regenerated, leaving a stale URL in `/etc/default/stellarindex-healthchecks`. Test-net VMs carry no URLs, so the series is absent there.

**Fix.**

1. Service under the check unhealthy: work that check's runbook first.
2. `rc=22`: repaste the URL from the dashboard into `/etc/default/stellarindex-healthchecks` (Ansible does not overwrite it after first install).
3. Verify `stellarindex_healthcheck_ping_last_success_unix` advances within one timer period (60 s heartbeats, 5 min smoke, 15 min SLA probe).

**Escalate:** none (ticket).

## stellarindex_notify_send_failure_ratio_high

**Symptom.** Failure ratio of `stellarindex_notify_sends_total{template,result="failed"}` above 50% for 15 min (ticket, `deploy/monitoring/rules/notify.yml`). `magic-link` (dashboard sign-in; the handler returns 200 regardless, so this counter is the only signal) or `signup-verify` stops delivering. Sessions and keys keep working.

**Check.**

```sh
# Which template is failing, and what's the ratio?
#   promql: sum by (template) (rate(stellarindex_notify_sends_total{result="failed"}[15m]))
#           / sum by (template) (rate(stellarindex_notify_sends_total[15m]))

# The send error is logged at the call site. Look for the mapped error class
# (ErrProviderRejected = 4xx, ErrTransient = 5xx/network, ErrInvalidMessage =
# our own validation).
ssh <api-host> 'journalctl -u stellarindex-api --since "30 min ago" --no-pager \
  | grep -iE "send magic link email|signup.?verif" | tail -30'
```

**Likely causes.** `notify: transient provider failure` (5xx, network; https://resend-status.com); `notify: provider rejected` (4xx: key rotated, domain unverified, bad From); `notify: invalid message` (template regression, a code bug); one failure in a near-empty window (`for: 15m` absorbs it).

**Fix.**

1. Provider outage: no local fix; note it in the incident channel.
2. 4xx: verify `STELLARINDEX_RESEND_API_KEY` is current and the sending domain is verified in Resend; redeploy the API with the corrected secret (never commit a key).
3. Invalid message: check recent changes to `internal/notify/templates.go` or the signup email body; roll back.
4. Verify `result="sent"` climbs and the ratio falls; send yourself a magic link.

**Escalate:** none (ticket).

### `stellarindex_notify_send_failed`

Fires on one failed send (`increase(...{result="failed"}[1h]) > 0`, `for: 0m`), clears an hour later. Read the call-site log for the error class: a lone `ErrTransient` is a retried blip, a repeating `ErrProviderRejected` is a bad address or key.

### `stellarindex_notify_send_rate_high`

Sent volume above 300/h. Login throttles cap each inbox and IP, not the total.

1. `sum by (template) (rate(stellarindex_notify_sends_total{result="sent"}[15m])) * 3600`.
2. `magic-link` dominating: look for many `/v1/auth/login` requests from many IPs in the API log; tighten the edge rate limit if abusive.
3. Another template dominating with no request volume: a send loop in our code; roll back.
4. A genuine spike (launch, press): silence for its duration.

## Related

- `configs/healthchecks/README.md`: per-binary heartbeat timers; the deadman says the pipeline is alive, the per-binary checks say which service died.
- Per-service runbooks if a service is down rather than unscrapeable.
- `ingestion.md#stellarindex_ledgerstream_tier_both_missing`: P1 page, live regardless of the registry gauge.
- [Alerts catalogue](../alerts-catalog.md).
