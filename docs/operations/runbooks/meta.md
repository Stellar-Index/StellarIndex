---
title: Runbook — meta (alerting pipeline self-health)
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — meta alerts

Alerts that watch Prometheus, Alertmanager and the exporters themselves. Rule file: `configs/prometheus/rules.r1/meta.yml` (group `stellarindex.meta`; multi-host twin `deploy/monitoring/rules/meta.yml`). Severities: `stellarindex_alertmanager_down`, `stellarindex_alertmanager_not_notifying` and the four `*_exporter_down` alerts are `page` (P1); `stellarindex_prometheus_scrape_failing`, `stellarindex_alertmanager_config_bad`, `stellarindex_alertmanager_notifications_failing` and `stellarindex_alertmanager_optional_receiver_dark` are `ticket`; `stellarindex_deadmansswitch` is `informational` by design (inverted semantics, below).

r1 shape: there are no `mon-01` / `mon-02` hosts (OBS-02). Prometheus (`prometheus.service`, scrape config `configs/prometheus/prometheus.r1.yml` installed to `/etc/prometheus/`) and Alertmanager (`prometheus-alertmanager.service`, NOT `alertmanager`, which is an inactive unit that reads as a false confirmation) are apt-installed systemd units on the one archival host; every scrape target is `localhost:<port>`. The `prometheus` ansible role and `monitoring.yml` playbook are the multi-host shape and are not runnable against r1.

Alertmanager config: source of truth `configs/alertmanager/alertmanager.r1.yml`; live file `/etc/prometheus/alertmanager.yml` (`0640 root:prometheus`, embeds webhook URLs). Secrets live in `/etc/default/alertmanager-secrets` (root:root, `0600`), never committed: `HEALTHCHECKS_DEADMANSSWITCH_URL`, `DISCORD_WEBHOOK_URL_PAGES`, `DISCORD_WEBHOOK_URL_ALERTS`, optional `DISCORD_WEBHOOK_URL_INFORMATIONAL` and `HEALTHCHECKS_ALERT_DELIVERY_URL`. `bash configs/alertmanager/apply.sh` renders (an empty OPTIONAL URL makes the renderer drop that receiver's `*_configs` block, leaving a no-op stub; an empty required URL is refused unless `ALERTMANAGER_ALLOW_EMPTY` waives it), amtool-validates, installs and reloads. Never hand-edit the rendered file; every r1 config change lands in the repo in the same PR. See `configs/alertmanager/README.md`.

Shared commands (host `ssh root@136.243.90.96`):

```sh
curl -s localhost:9090/-/healthy; curl -s localhost:9093/-/healthy
journalctl -u prometheus-alertmanager -n 100 --no-pager
amtool check-config /etc/prometheus/alertmanager.yml
# delivery rate on the webhook integration; steady state ~30/hour
curl -s 'localhost:9090/api/v1/query?query=rate(alertmanager_notifications_total\{integration="webhook"\}[5m])*3600'
```

Alertmanager down, not notifying and notifications failing are three distinct failures: process gone, process up but fanning out to nobody, trying and being refused. They exist because of a real outage: between 2026-07-29 06:24 and 2026-08-29 22:14 Alertmanager delivered zero notifications through any integration, and nothing reported it.

## At a glance

- [`stellarindex_prometheus_scrape_failing`](#stellarindex_prometheus_scrape_failing) (ticket): a scrape target is down 2 min
- [`stellarindex_alertmanager_config_bad`](#stellarindex_alertmanager_config_bad) (ticket): config reload failed 5 min
- [`stellarindex_alertmanager_down`](#stellarindex_alertmanager_down) (page): Alertmanager unscraped or absent
- [`stellarindex_alertmanager_not_notifying`](#stellarindex_alertmanager_not_notifying) (page): zero webhook notifications in 15 min
- [`stellarindex_alertmanager_notifications_failing`](#stellarindex_alertmanager_notifications_failing) (ticket): delivery attempted and refused
- [`stellarindex_alertmanager_optional_receiver_dark`](#stellarindex_alertmanager_optional_receiver_dark) (ticket): optional receiver has no URL
- [`stellarindex_redis_exporter_down`](#stellarindex_redis_exporter_down), [`postgres`](#stellarindex_postgres_exporter_down), [`pgbackrest`](#stellarindex_pgbackrest_exporter_down), [`minio`](#stellarindex_minio_exporter_down) (page): exporter gone, dependent alerts blind
- [`stellarindex_deadmansswitch`](#stellarindex_deadmansswitch) (inverted): page when it STOPS firing
- [`stellarindex_metrics_registry_absent`](#stellarindex_metrics_registry_absent)
- [`stellarindex_healthcheck_ping_undelivered`](#stellarindex_healthcheck_ping_undelivered)
- [`stellarindex_notify_send_failure_ratio_high`](#stellarindex_notify_send_failure_ratio_high)

## stellarindex_prometheus_scrape_failing

Fires on `up{job=~"stellarindex-api|stellarindex-indexer|stellarindex-aggregator|node_exporter|prometheus|caddy|galexie"} == 0` for 2 min (hyphenated job names of `prometheus.r1.yml`; the multi-host twin uses underscored ones). Visibility into that subsystem is lost; the subsystem itself may be fine, often the exporter is the problem. Typical MTTR 5-30 min.

Deliberately excluded: `redis_exporter`, `postgres_exporter`, `pgbackrest_exporter` and `minio` have their own page-severity alerts (see [exporter section](#stellarindex_redis_exporter_down)), because an exporter outage silently blinds every alert that depends on it.

Diagnosis:

```sh
# Prometheus's view of the failing target; lastError says why (refused, TLS, 404, parse error)
curl -s http://localhost:9090/api/v1/targets?state=active | \
  jq '.data.activeTargets[] | select(.health != "up") | {job: .labels.job, instance: .labels.instance, lastError: .lastError}'
curl -s http://localhost:<port>/metrics | head
systemctl list-units | grep -i exporter      # unit names vary by package
systemctl status <exporter-unit> --no-pager | head -15
```

Causes and fixes:

1. Host rebooted or unit restarted (ansible upgrades). Prometheus's static-config discovery re-resolves on each scrape, so recovery is bounded by the scrape interval. `for: 2m` absorbs this; if it fires the unit is staying down.
2. Exporter crash: `systemctl restart <exporter>`.
3. Static-config drift (target added to inventory without re-applying the role, or removed but still scraped): on r1 install the updated `configs/prometheus/prometheus.r1.yml` to `/etc/prometheus/` and SIGHUP the unit; multi-host re-apply the `prometheus` role.
4. Auth drift: exporter credentials rotated in vault without re-applying; Prometheus gets 401. Rotate the vault entry, re-apply.
5. Firewall: on r1 every scrape is localhost to localhost, so only a host-local nft change bites; multi-host, open ingress from monitoring hosts to the metrics port.
6. If the target service is genuinely down (not just unscrapeable), cross-reference that service's own alerts.

Verify: `up` returns to 1 and metrics resume. False positives: a Prometheus reload during a config change drops all targets briefly (the reason for `for: 2m`); between `systemctl start` and the first /metrics serve `up==0` until the next scrape.

## stellarindex_alertmanager_config_bad

Fires on `alertmanager_config_last_reload_successful == 0` for 5 min. A reload after a config push failed, so changes since the last good load are NOT live; existing routes keep working from the previous in-memory config, new routes go nowhere. Log shows `error loading config: ...`; a recent edit or hand apply whose new route does not fire is the other symptom. Typical MTTR 5-30 min. The alert is live on r1 via the `alertmanager` self-scrape job in `prometheus.r1.yml` (`localhost:9093`).

Diagnosis: `journalctl -u prometheus-alertmanager -n 100 --no-pager | grep -iE 'reload|error'` and `amtool check-config /etc/prometheus/alertmanager.yml`.

Causes:

1. YAML typo (`amtool check-config` catches it). A malformed edit that breaks `apply.sh`'s block-stripper indentation assumptions can instead produce a validating but receiver-less config (the pre-#275 incident class): a silent no-fanout, not a `config_bad` firing.
2. Template-expansion error: malformed `{{ ... }}` parses fine; reference errors only fire at send time, and watch for silent "expanded to empty string".
3. Secret resolution is NOT a load failure on r1. An unset optional (or explicitly waived) secret drops that receiver's `*_configs` block (no-op stub): alerts accumulate in the AM UI but never reach Discord/Healthchecks. If fan-out is missing but this alert is green, check the env file for empty URLs and re-run `apply.sh` (see [not_notifying](#stellarindex_alertmanager_not_notifying)).
4. Version skew: new AM binary vs old config syntax, or a hand-upgraded amtool disagreeing with the running binary (the apt package pins the distro's AM version).

Mitigation:

1. Validate the checked-in source: `ALERTMANAGER_SECRETS=/dev/null bash configs/alertmanager/apply.sh --check-only`; fix syntax in `alertmanager.r1.yml`.
2. Confirm `/etc/default/alertmanager-secrets` sets the three required URLs (`apply.sh` refuses an empty one unless `ALERTMANAGER_ALLOW_EMPTY` waives it; only the optional receivers silently become stubs).
3. Apply: `bash configs/alertmanager/apply.sh` (render, amtool-validate, install `0640 root:prometheus`, reload).
4. Manual reload: `systemctl reload prometheus-alertmanager`. Do not `curl -XPOST http://localhost:9093/-/reload`: the apt unit runs without `--web.enable-lifecycle` (`/etc/default/prometheus-alertmanager` carries only `ARGS="--cluster.listen-address="`, per `configs/prometheus/README.md`).
5. Verify `alertmanager_config_last_reload_successful == 1`; the alert clears within one evaluation interval.

CI guard (#275): the `monitoring-rules` job runs `apply.sh --check-only` on both render branches (empty URLs exercising the stripper stub path, dummy URLs exercising substitution), validating the rendered config, so a PR that breaks either branch fails before reaching r1.

False positive: `last_reload_successful` is 0 until the first load completes, so a cold start can trip briefly; `for: 5m` absorbs it.

Dependency: this alert relies on Alertmanager being up enough to serve metrics; a totally broken Alertmanager is caught by [deadmansswitch](#stellarindex_deadmansswitch) and [scrape_failing](#stellarindex_prometheus_scrape_failing).

Future multi-host pair (not runnable today): live config at `/etc/alertmanager/alertmanager.yml` rendered from the role's `alertmanager.yml.j2`; push via the prometheus role (handler reloads); diff the live config across the pair (ADR-0008 section 3; `diff <(ssh root@mon-01 cat /etc/alertmanager/alertmanager.yml) <(ssh root@mon-02 cat /etc/alertmanager/alertmanager.yml)`, they must agree); verify `alertmanager_config_last_reload_successful == 1` on both.

## stellarindex_alertmanager_down

Fires (page) on `up{job="alertmanager"} == 0 or absent_over_time(up{job="alertmanager"}[10m]) == 1` for 5 min. No alert in the system can be delivered, including this one: treat the Prometheus UI (`http://localhost:9090/alerts`), not Discord, as the source of truth. The `absent_over_time` arm is needed because if the job is dropped from service discovery `up == 0` alone matches nothing and the alert would be unfireable (`sum()` is deliberately not used for the same reason).

Triage:

```sh
systemctl status prometheus-alertmanager    # not `alertmanager`
journalctl -u prometheus-alertmanager -n 80 --no-pager
ss -lntp | grep 9093
```

| Symptom in the journal | Cause | Action |
| --- | --- | --- |
| `error loading config` + exit | bad config installed by hand | restore `/etc/prometheus/alertmanager.yml` from the last good copy, then re-apply from a checkout |
| OOM-killed | host memory pressure | check `stellarindex_host_memory_high`; AM's own footprint is small, find the real consumer |
| Port 9093 already bound | stale process survived a restart | `systemctl stop`, confirm with `ss -lntp`, then start |
| Scrape target absent, unit healthy | Prometheus scrape config lost the job | check `job_name: alertmanager` in `/etc/prometheus/prometheus.yml` |

Recover: `systemctl restart prometheus-alertmanager`, then `curl -s localhost:9093/api/v2/status | head -c 300`. A running process is not proof of fan-out: check the webhook delivery rate (shared commands). If the process is up but the rate stays zero, see [not_notifying](#stellarindex_alertmanager_not_notifying).

## stellarindex_alertmanager_not_notifying

Fires (page) when `sum(increase(alertmanager_notifications_total{job="alertmanager",integration="webhook"}[15m])) == 0` or that series is absent for 15 min, sustained 10 min. Both arms are needed: during the 31-day outage the webhook series was present and flat at 0 while the discord series was entirely absent. `sum()` with no `by` is load-bearing (GH-1173): the webhook integration backs two receivers (`deadmansswitch`, `alert-delivery-failure`), each its own series; unaggregated, the idle `alert-delivery-failure` series would sit at 0 permanently and page forever once wired, muting the deadman via the `component: meta` inhibit rule.

Why zero is always wrong: `stellarindex_deadmansswitch` fires permanently with `repeat_interval: 1m`, so the webhook integration produces roughly 30 notifications an hour forever (measured steady state on r1: `30.0/hour`). Zero means notifications are not leaving the process, never a quiet estate. While this alert fires it cannot reach you, so treat the Prometheus UI (`http://localhost:9090/alerts`), not Discord, as the source of truth; the external Healthchecks.io check on the deadman's switch is the out-of-band path that should have paged first (see step 4).

1. Confirm and find which leg:

   ```sh
   curl -s localhost:9093/api/v2/status | python3 -c 'import sys,json;print(json.load(sys.stdin)["config"]["original"])' | grep -nE '_configs:|- name:'
   curl -s 'localhost:9090/api/v1/query?query=alertmanager_notifications_total' \
     | python3 -c 'import sys,json;[print(r["metric"].get("integration"), r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'
   ```

   A receiver with `- name: X` and no following `*_configs:` block accepts alerts and delivers to nobody; that is the failure. Two may legitimately look like that: `silent` (black hole on purpose, nothing routes to it now) and `chat-informational` when `DISCORD_WEBHOOK_URL_INFORMATIONAL` is unset (`informational` has routed there since 2026-09-08; `apply.sh` treats that URL as optional and degrades to a stub; `deadmansswitch`, `chat-page` and `chat-default` are the three it refuses to install empty). Anything else with a bare `- name:` is the bug.

2. Overwhelmingly likely cause is an empty URL. Names and lengths only, never values:

   ```sh
   awk -F= '/^(HEALTHCHECKS|DISCORD)/{printf "%s len=%d\n", $1, length($2)}' /etc/default/alertmanager-secrets
   ```

   Any `len=0` is the bug. AM does not error on an empty URL: `apply.sh` drops the `*_configs` block, the reload succeeds, which is why `stellarindex_alertmanager_config_bad` stays silent.

3. Fix: repopulate the URL(s) in `/etc/default/alertmanager-secrets`, then `bash configs/alertmanager/apply.sh`. It refuses to install a config whose receivers deliver to nobody, probes each URL for a live 2xx before installing, and reads the running config back to assert the delivery blocks exist. If it exits non-zero it is reporting the real problem; do not use `ALERTMANAGER_ALLOW_EMPTY` unless a receiver is genuinely meant to be dark. Confirm the webhook rate climbs back toward ~30/hour within a few minutes.

4. If the deadman's switch did not page you either, the out-of-band path is also broken (second incident). An empty `HEALTHCHECKS_DEADMANSSWITCH_URL` disarms it the same way. Check the Healthchecks.io check still exists, has a grace period and an escalation channel; that configuration is off-box and no in-repo check proves it.

## stellarindex_alertmanager_notifications_failing

Fires (ticket) when `increase(alertmanager_notifications_failed_total{job="alertmanager"}[15m]) > 0` for 5 min. The healthy-failure case: Alertmanager is trying and the far end rejects it (contrast [not_notifying](#stellarindex_alertmanager_not_notifying)). Steady state on r1 is 0 failures across every integration and reason, so any sustained increase is real.

1. Identify integration and reason:

   ```sh
   curl -s 'localhost:9090/api/v1/query?query=alertmanager_notifications_failed_total' \
     | python3 -c 'import sys,json;[print(r["metric"].get("integration"), r["metric"].get("reason"), r["value"][1]) for r in json.load(sys.stdin)["data"]["result"] if float(r["value"][1])>0]'
   journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -i 'notify\|error'
   ```

   | `reason` | Meaning | Usual cause |
   | --- | --- | --- |
   | `clientError` (4xx) | far end rejected us | revoked Discord webhook, deleted Healthchecks check, wrong UUID |
   | `serverError` (5xx) | far end broken | Discord or Healthchecks outage, usually self-clearing |
   | `contextCanceled` / timeout | we gave up | host network, DNS, egress firewall |
   | `other` | untyped error | what a Discord rejection looks like: the notifier reports `unexpected status code NNN`, so `integration="discord"` never lands in `clientError`. Read the code from the journal, not the metric |

   A `clientError` on the `webhook` integration is serious: Healthchecks.io returns 404 for an unknown UUID, so it means the deadman's switch is pinging a check that no longer exists and the alarm of last resort is disarmed while looking healthy.

2. Discord with status 400 (payload):

   ```sh
   journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -c 'unexpected status code 400'
   journalctl -u prometheus-alertmanager --since -1h --no-pager | grep -o 'num_alerts=[0-9]*' | sort | uniq -c
   ```

   A 400 with body `{"embeds": ["0"]}` means an invalid embed, almost always size: description max 4096 characters, title 256, together 6000. Alertmanager 0.26 does not truncate and treats a 400 as unrecoverable (one attempt, no retry), so the receiver is dead for every alert routed to it. The 2026-09-07 shape: `reflector-dex` stalled, 22 `stellarindex_oracle_stale` alerts landed in one `group_by: [alertname, severity]` group, the then-unbounded `range .Alerts` template rendered 9319 characters, and `chat-default` returned 400 every 5 minutes for 11 hours, dropping every other ticket alert including this one. Both Discord templates are now bounded (at most three alerts, a "... and N more in this group" line, `printf "%.Ns"` on every interpolated field); measured worst case is 2257 characters at any group size. On a fresh 400, re-measure before assuming the far end changed (read-only, uses AM's own template engine):

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

   `wc -m` counts characters (UTF-8 locale), which is Discord's unit; the templates emit em dashes and ellipses. Anything approaching 4096 is a template defect, not a big incident.

3. Fix: for a revoked or mistyped credential repopulate `/etc/default/alertmanager-secrets` and run `bash configs/alertmanager/apply.sh` (it probes every URL for a live 2xx and refuses a revoked one). For a far-end outage confirm on the provider's status page and let it clear.

4. Verify (use `-G --data-urlencode`: Prometheus answers an unencoded `[` with an empty body, which the pipeline reports as a JSON decode error). Expect `0.0`:

   ```sh
   curl -s -G localhost:9090/api/v1/query \
     --data-urlencode 'query=increase(alertmanager_notifications_failed_total[15m])' \
     | python3 -c 'import sys,json;print(sum(float(r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]))'
   ```

Why you may never see this alert: `ticket` severity routes to `chat-default` (Discord), so when Discord is the failing integration the alert travels the broken path (the 11-hour window above). The routing tree therefore also sends it to `alert-delivery-failure`, a Healthchecks.io webhook on a separate check, with `continue: true`. Arm it:

1. Create a new Healthchecks.io check named for delivery failures, not the deadman's switch (pinging the deadman's `/fail` would make one signal mean both "Prometheus/AM dead" and "chat delivery refused" and send you to the wrong runbook). Period and grace 365 days (pinged only when something is wrong). Enable HTTP body filtering (API `filter_http_body`, not `filter_body`, which is email only) with failure keyword `"status":"firing"` and success keyword `"status":"resolved"`; without it every AM POST is a success ping and a failure never alarms. Notify by email, not Discord. Test with one POST of each body; the check goes down, then up.
2. Add the ping URL to `/etc/default/alertmanager-secrets` as `HEALTHCHECKS_ALERT_DELIVERY_URL`.
3. `bash configs/alertmanager/apply.sh` prints `optional receiver 'delivery-failure' is wired` when set and `... is DARK` on every run until it is.

Until step 2 is done the receiver is a stub and this alert reaches chat only. Wiring is safe because of the `sum()` in not_notifying (GH-1173).

## stellarindex_alertmanager_optional_receiver_dark

Fires (ticket) when `stellarindex_alertmanager_optional_receiver_dark == 1` for 1 h. `apply.sh` installs an optional receiver with an empty URL rather than refusing, writes `stellarindex_alertmanager_optional_receiver_dark{receiver}` to the textfile collector, and this alert tickets while any optional receiver is dark: `delivery-failure` (`HEALTHCHECKS_ALERT_DELIVERY_URL`) or `informational` (`DISCORD_WEBHOOK_URL_INFORMATIONAL`). Its `*_configs` block was dropped, so whatever routes there is accepted and discarded; for `delivery-failure` a refused chat delivery has no out-of-band alarm.

Difference from [notifications_failing](#stellarindex_alertmanager_notifications_failing): nothing is being refused; the receiver was installed with no URL. The body is the same procedure: the `receiver` label tells you which one. Set its URL in `/etc/default/alertmanager-secrets` and re-run `bash configs/alertmanager/apply.sh` (arming steps for `delivery-failure` are in that section). An unset `informational` URL is a deliberate no-op if you want it dark; otherwise set it.

## stellarindex_redis_exporter_down

Fires (page) on `up{job="redis_exporter"} == 0 OR absent_over_time(up{job="redis_exporter"}[5m]) == 1` for 2 min. The same shape applies to all four exporter alerts: `up == 0` catches scraped-but-failing, `absent_over_time` catches the target disappearing from service discovery. Typical MTTR 5-15 min.

Why it exists (F-0085, audit-2026-05-26): in the 2026-05-10 SEV-2 cascade `stellarindex_redis_writes_blocked` (`redis_rdb_last_bgsave_status == 0`) never fired because `redis_exporter` itself was down; the metric was absent, the alert evaluated `absent_gauge == 0` (no_data). An exporter outage cascade-blinds every dependent alert: the subsystem can be on fire and nobody is paged. Symptoms: `up{job="<exporter>"} == 0` or absent 2+ min; dependent alerts unexpectedly silent (not OK, not evaluating); dashboards on its metrics go flat.

All r1 exporters except minio are installed by `configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml` (groups A/B/C). The `redis-sentinel` role's `tasks/07-monitoring.yml` ships a different tarball-based redis_exporter but is not applied to r1; do not look there.

Diagnosis (all four):

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

Mitigation (within 15 min): `systemctl restart <unit>`. If it will not start, read the journal: stale auth (postgres DSN drift, MinIO bearer-token file missing or rotated), permissions regression on the read socket or data dir, or the underlying service itself down (then follow that service's runbook for the real RCA). Verify: the curl returns 200 with metrics, `up{job="<job>"}` is 1, the alert auto-resolves in about 2 min, and the dependent family is present again (for redis, `redis_rdb_last_bgsave_status` present and `== 1`).

RCA capture: unit journal for the 30 minutes before the alert (`journalctl -u <unit> --since "30 min ago"`), the exporter's endpoint response just before recovery, concurrent pressure (`df -h`, `free -m`, `dmesg -T | tail -50`), and whether the underlying service was also down.

False positives: brief restarts during planned exporter upgrades (silence via amtool); MinIO bearer-token rotation without a Prometheus reload (Prometheus must reload its token file).

Redis specifics: Debian unit `prometheus-redis-exporter`, port 9121, bound to `127.0.0.1` via `WEB_LISTEN_ADDRESS` in `/etc/default/prometheus-redis-exporter` (group A). Blinds `cache.yml` (`configs/prometheus/rules.r1/cache.yml`), `stellarindex_redis_writes_blocked` and any cache-miss/latency rule on `redis_*`. Downstream runbook: `cache.md#stellarindex_redis_writes_blocked`.

## stellarindex_postgres_exporter_down

Same diagnosis and mitigation as [redis_exporter_down](#stellarindex_redis_exporter_down); expression identical with `job="postgres_exporter"`.

Specifics: Debian unit `prometheus-postgres-exporter`, port 9187, bound to `127.0.0.1` via `PG_EXPORTER_WEB_LISTEN_ADDRESS` (group B). Reads `DATA_SOURCE_NAME` from `/etc/default/prometheus-postgres-exporter`: peer auth on the local Unix socket as the package's own `prometheus` role, which needs `pg_monitor`. Blinds every `pg_*` alert in `configs/prometheus/rules.r1/storage.yml`, including `stellarindex_timescale_primary_down`, `stellarindex_timescale_replica_lag`, `stellarindex_timescale_lock_table_pressure` and `stellarindex_timescale_connections_saturated`.

## stellarindex_pgbackrest_exporter_down

Same diagnosis and mitigation as [redis_exporter_down](#stellarindex_redis_exporter_down). Difference in the rule: the absent arm only counts when the exporter has ever reported (`absent_over_time(up{job="pgbackrest_exporter"}[5m]) == 1 and on () count(stellarindex_pgbackrest_backup_last_rc) > 0`); `up == 0` fires unconditionally. An exporter that is UP but has no stanza to report is the sibling gap, owned by `stellarindex_pgbackrest_backup_metrics_absent`.

Specifics: unit `pgbackrest_exporter`, port 9854 (group C; pinned upstream tarball, no Debian package; the role fails fast unless `pgbackrest_exporter_release_sha256` is set in inventory). Blinds `stellarindex_timescale_backup_failed` and `stellarindex_timescale_backup_none_24h` (both read `pgbackrest_backup_since_last_completion_seconds`, which only this exporter publishes); a 24h RPO breach during downtime would not surface until it returned.

## stellarindex_minio_exporter_down

Same diagnosis and mitigation as [redis_exporter_down](#stellarindex_redis_exporter_down), with `up{job="minio"}`; the scrape target is MinIO itself, not a separate exporter.

Specifics: unit `minio`, port 9000. Bearer token at `/etc/prometheus/minio.token`; a missing file or a 401 is the common cause, regenerate per [credential-rotation.md](../credential-rotation.md#prometheus-bearer-token-regen-inv-0981inv-1144--now-codified). The other common cause is MinIO being stopped. Blinds the galexie-archive, archive-completeness, archive-publish and object-storage usage/capacity families; Galexie writes ledger metadata to MinIO and the indexer reads it back (ADR-0002), so prolonged invisibility is high risk.

## stellarindex_deadmansswitch

Inverted semantics: fires constantly by design (`expr: vector(1)`, `for: 0s`); you page when it STOPS. The alert being visible in Prometheus is the positive case (pipeline healthy). Its `severity` label is `informational` on purpose: the Alertmanager routing tree matches on `alertname`, not severity, to send it to the watchdog receiver, and `informational` keeps it out of the page/ticket fanout. P1 when it stops, escalated by the external watchdog. If it stops, the primary alerting pipeline is lost and every other alert is invisible. MTTR is however long it takes to restore Prometheus or Alertmanager (minutes to an hour).

How it works: routed via `configs/alertmanager/alertmanager.r1.yml` to a Healthchecks.io `https://hc-ping.com/<uuid>` check that expects a heartbeat every repeat interval; if it stops hearing, it pages on a separate channel independent of our Alertmanager. The checked-in config holds only the placeholder `${HEALTHCHECKS_DEADMANSSWITCH_URL}`; the real URL is never committed. To rotate or fix the watchdog URL edit `HEALTHCHECKS_DEADMANSSWITCH_URL` in `/etc/default/alertmanager-secrets` and re-run `apply.sh` (format `'https://hc-ping.com/<uuid>'`).

Symptoms: a secondary-channel page "deadmansswitch heartbeat missed"; Prometheus/Alertmanager dashboards may look green or offline; the primary on-call tool is silent.

Diagnosis:

```sh
curl -s localhost:9090/-/healthy; curl -s localhost:9093/-/healthy
# Healthchecks.io dashboard: when did the hc-ping.com/<uuid> check last hear from us?
amtool --alertmanager.url=http://localhost:9093 config routes show   # route still in the RENDERED config?
```

Causes:

1. Prometheus down or unreachable (cannot evaluate `vector(1)`).
2. Alertmanager down or unreachable (cannot route it).
3. Network path to the watchdog broken (DNS, proxy, TLS to `hc-ping.com`).
4. Watchdog URL empty or wrong in the rendered config: an empty `HEALTHCHECKS_DEADMANSSWITCH_URL` is refused by `apply.sh` unless `ALERTMANAGER_ALLOW_EMPTY` waives it, and then renders a no-op stub receiver (valid config, zero pings). Check the secrets file and re-run `apply.sh`.
5. Someone silenced the alert in Alertmanager; it should never be silenced.
6. The `stellarindex.meta` rule group is disabled (misconfig or rule-load error); `alertmanager_config_last_reload_successful` and `prometheus_rule_group_iterations_total` tell you.

Mitigation: find which component is down; restore it (`systemctl status prometheus prometheus-alertmanager --no-pager`; `systemctl restart prometheus` or `prometheus-alertmanager` if wedged); watch the Healthchecks.io dashboard for heartbeats to resume. Do NOT ack the secondary page until the primary channel is verified end to end; send a test alert through Alertmanager if in doubt. Verified when the watchdog's last ping is recent and the check is "up", other alerts route, and a test alert fires and clears.

False positives: Healthchecks.io provider outage (cross-check from an independent network such as your phone); an egress firewall change blocking `hc-ping.com` (whitelist the hostname explicitly).

## stellarindex_metrics_registry_absent

_Source page `meta.md#stellarindex_metrics_registry_absent`: status draft, severity P3, last verified 2026-07-16._


### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_metrics_registry_absent` |
| Severity | P3 (informational — monitoring-coverage gap, not an active outage) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/metrics-registry.yml` |
| Typical MTTR | code change + deploy (this is a wiring regression, not an incident) |
| Impact | A component is running WITHOUT a Prometheus Registry, so the metrics it would export are never registered and any alert built on them can NEVER fire. A silent hole in monitoring coverage. |

### Background (audit-2026-07-16 C4-4)

Some components accept an optional `*prometheus.Registry` and, when it
is nil, simply skip registering their metrics. That is convenient for
tests but dangerous in production: an alert whose source metric is
never registered is DEAD — it evaluates against "no data" forever and
can never fire, so the failure it was meant to catch goes unnoticed.

`internal/obs/metrics.go` exports the gauge
`stellarindex_metrics_registry_present{component}` — set to `1` at boot
when the named component received a Registry, `0` when it is running
Registry-less. This alert fires on a present `0`. An ABSENT series
means "this binary doesn't use the component" and is intentionally not
alerted.

### The known case: `component="ledgerstream"`

`internal/ledgerstream` registers its SDK `BufferedStorageBackend`
buffer metrics (`buffer_fetch_latency_seconds` etc., via the SDK's
`WithMetrics` / `ApplyLedgerMetadata`) ONLY when `Config.Registry != nil`.

The production builder `pipeline.LedgerstreamConfig` leaves `Registry`
nil **on purpose**: the live indexer calls `ledgerstream.Stream`
repeatedly (archive range → live tail → each ch-live-catchup
tip-extend), and the SDK's metric registration is not idempotent — the
second call with the same registry panics with a duplicate-registration
error. Leaving the registry nil is the current way to avoid that panic.

Consequence: the SDK buffer metrics (`buffer_fetch_latency_seconds`
etc.) are not exported in production. That is a low-value operational
coverage gap, not a dead page.

**2026-08-05: the alert rule now EXCLUDES `component="ledgerstream"`**
— it fired continuously for a week on this documented accepted state,
which is alert-board noise, while staying unable to distinguish it
from a new regression. The rule remains armed for every OTHER
component. Queued real fix: a swappable gatherer-bridge collector —
give the SDK a fresh sub-registry per `Stream` call and expose the
CURRENT one through the main registry via an unchecked collector that
converts `Gather()` output to const metrics — which makes repeated
registration safe without SDK changes; remove the exclusion in the
same PR that lands it.

> **NOTE (W5-mon-3):** this alert USED to also mean the
> `TieredDataStore` metrics were dead and the ledgerstream-tier
> `both_missing` P1 page was inert. That is **no longer true.**
> `stellarindex_ledgerstream_tier_read_total` and
> `stellarindex_ledgerstream_cold_read_duration_seconds` are now
> `internal/obs` package-level metrics registered unconditionally at
> boot, so the `both_missing` page is **live in production regardless of
> this gauge's value**. This alert now flags only the SDK buffer-metric
> coverage gap.

### What to do

1. Confirm which component: check the `component` label on the firing
   series (`stellarindex_metrics_registry_present == 0`).
2. For `ledgerstream`, this is the known state, not a new regression,
   and it now affects only the SDK buffer metrics (the `both_missing`
   page is unaffected — see the note above). If you want the buffer
   metrics too, the fix is a code change, not an ops action:
   - Make the SDK metric registration idempotent — gate the SDK
     `WithMetrics` / `ApplyLedgerMetadata` calls behind a package-level
     `sync.Once` or an `AlreadyRegisteredError`-tolerant register, so
     repeated `Stream` calls don't panic.
   - Then wire `obs.Registry` (+ a `RegistryNamespace`) through
     `pipeline.LedgerstreamConfig`.
   - After deploy, `stellarindex_metrics_registry_present{component="ledgerstream"}`
     flips to `1` and this alert clears.
3. For any other component that starts reporting `0`, treat it as a
   wiring regression: something stopped passing the Registry into that
   component's constructor. Restore the wiring.

### Verifying the fix

After the change, `curl` the indexer's `/metrics` and confirm:

- `stellarindex_metrics_registry_present{component="ledgerstream"} 1`
- the SDK buffer metric `stellarindex_ledgerstream_buffer_fetch_latency_seconds`
  (or the SDK's namespaced equivalent) is present.

(`stellarindex_ledgerstream_tier_read_total` is present independent of
this gauge — it is registered at boot regardless.)


## stellarindex_healthcheck_ping_undelivered

**Runbook — Healthchecks.io ping delivery**

_Source page `meta.md#stellarindex_healthcheck_ping_undelivered`: status ratified, severity P3, last verified 2026-10-06._

Healthchecks.io marks a check down by silence, so a stopped service and a
ping that never left this host look identical to it. `configs/healthchecks/hc-ping.sh`
(sourced by `heartbeat.sh`, `smoke.sh`, `sla-probe.sh` in that directory)
records every failed delivery in the journal and in node_exporter's textfile
collector (`/var/lib/node_exporter/textfile_collector/hc_ping_<check>.prom`).
While the alert fires, a Healthchecks.io "down" notice for that check is about
this host's egress, not the service.

Impact: none to API consumers. Severity ticket (P3), typical MTTR 10 min.
Wiring: `configs/ansible/roles/archival-node/tasks/17-stellarindex-healthchecks.yml`.
Test: `internal/ops/chops/healthcheck_ping_delivery_test.go`.
Companion runbooks (the checks whose emails this qualifies):
[`sla-probe.md#stellarindex_api_smoke_failing`](sla-probe.md#stellarindex_api_smoke_failing),
[`sla-probe.md#stellarindex_api_smoke_stale`](sla-probe.md#stellarindex_api_smoke_stale).

### At a glance

- [`stellarindex_healthcheck_ping_undelivered`](#stellarindex_healthcheck_ping_undelivered)

### stellarindex_healthcheck_ping_undelivered

Trips (identical in `deploy/monitoring/rules/healthcheck-ping.yml` and
`configs/prometheus/rules.r1/healthcheck-ping.yml`), severity `ticket`:

```
increase(stellarindex_healthcheck_ping_failures_total[15m]) > 0
and
(time() - stellarindex_healthcheck_ping_last_success_unix) > (15 * 60)
for: 5m
```

Two clauses so one lost ping stays silent: a blip that `hc_ping`'s own
`--retry 2` did not absorb but that cleared on the next timer firing meets the
first clause only. A check that never delivered has `last_success = 0`, so the
second clause is true by construction (a wrong URL from install is reported).

Symptoms: `stellarindex_healthcheck_ping_failures_total{check="…"}` rising for
at least 15 min, `stellarindex_healthcheck_ping_last_success_unix` for that
check not advancing, and possibly a Healthchecks.io "down" email while the
service is fine.

Diagnose (≤ 5 min):

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

Fix (≤ 15 min):

- Service under the check unhealthy: this alert is secondary; work that
  check's own runbook first.
- `rc=6` DNS: check `/etc/resolv.conf` and that the host resolver is reachable.
- `rc=7` or `rc=28` egress: check outbound HTTPS and whether hc-ping.com is up
  (<https://status.healthchecks.io>).
- `rc=22`: hc-ping.com rejected the request. Usual cause is a check deleted or
  regenerated on the dashboard, leaving a stale URL in
  `/etc/default/stellarindex-healthchecks`. Repaste the URL from the dashboard;
  the file is operator-populated and Ansible does not overwrite it after first
  install.
- Verify: `stellarindex_healthcheck_ping_last_success_unix` advances within one
  timer period (60 s heartbeats, 5 min smoke, 15 min SLA probe).

Root cause: capture the curl exit code and check name from the journal, plus
`stellarindex_healthcheck_ping_failures_total` at the start and end of the
window. Climbed then flat on its own = hc-ping.com or network; climbing from a
config change = the URL.

False positives:

- A just-reimaged host: `hc_ping` treats an empty URL as nothing to deliver and
  stays silent, but a wrong non-empty URL gives `rc=22` legitimately.
- Test-net VMs carry no Healthchecks.io URLs, so the series should be absent
  there, not failing.


## stellarindex_notify_send_failure_ratio_high

_Source page `meta.md#stellarindex_notify_send_failure_ratio_high`: status ratified, severity P2, last verified 2026-08-25._


### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_notify_send_failure_ratio_high` |
| Severity | P2 (ticket — user-facing auth flows stop delivering, but existing sessions/keys are unaffected) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/notify.yml` (counter from `internal/notify` call sites) |
| Typical MTTR | 15 min (credential/domain fix) – provider-dependent (Resend outage) |
| Impact | The named `template` stops delivering: `magic-link` → no new dashboard sign-ins; `signup-verify` → API-signup confirmations don't arrive (the key still works, but `email_verified` never flips). Existing sessions and API keys keep working. |

### Symptoms

- `stellarindex_notify_sends_total{template="…",result="failed"}` climbing while
  `result="sent"` is flat.
- The failure ratio for a template exceeds 50% for 15+ minutes.
- Users report "I never got the sign-in email" / "my confirmation link never
  arrived".

### What this metric watches

`internal/notify` is the Resend client behind two mail paths and only two:

- `magic-link` — the dashboard sign-in email (`internal/api/v1/dashboardauth`).
  The login handler deliberately returns `200` whether or not the send
  succeeds (so an attacker can't use the response to confirm an email exists),
  so **the counter is the only signal the mail failed**.
- `signup-verify` — the API-signup confirmation email
  (`cmd/stellarindex-api` `signupVerifyEmailerAdapter`).

Price alerts deliver via **webhooks**, not mail — they are unaffected by a mail
outage and are watched separately.

### Quick diagnosis (≤ 5 min)

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

| Log / error class | Likely cause |
| ----------------- | ------------ |
| `notify: transient provider failure` (5xx / network) | Resend outage or network egress problem — check https://resend-status.com |
| `notify: provider rejected` (4xx) | API key rotated/invalid, sending domain unverified, or a bad From address |
| `notify: invalid message` | A template/rendering regression produced an empty subject/body — a code bug, not a provider issue |

### Mitigation

- [ ] **Provider outage (transient/5xx)**: confirm on Resend's status page. If
  it's them, there is no local fix — the counter recovers when they do. Note it
  in the incident channel so support can tell affected users to retry.
- [ ] **Credential / domain (4xx)**: verify `STELLARINDEX_RESEND_API_KEY` is set
  and current, and that the sending domain is still verified in the Resend
  dashboard. Rotating the key is a **separate operational action** (do not
  commit a key); redeploy the API with the corrected secret.
- [ ] **`invalid message` (our bug)**: this is a rendering/validation
  regression, not a provider problem — check recent changes to
  `internal/notify/templates.go` or the signup email body; roll back if needed.
- [ ] **Verification**: `result="sent"` resumes climbing and the ratio falls
  back below the threshold. Send yourself a magic link to confirm end-to-end.

### Known false-positive patterns

- **Very low mail volume**: the ratio is computed over a 15m window; a single
  failure in an otherwise-empty window can briefly spike the ratio. The
  `for: 15m` dwell absorbs one-off blips — a sustained firing is real.

### `stellarindex_notify_send_failed` — any failed send in 1h

Fires on one failed send (`increase(...{result="failed"}[1h]) > 0`, `for: 0m`)
and clears an hour later. It exists for rare failures the ratio and
sustained alerts cannot see. Read the call-site log for the mapped error
class (see Quick diagnosis); a lone `ErrTransient` is a retried blip, a
repeating `ErrProviderRejected` is a bad address or key.

### `stellarindex_notify_send_rate_high` — sent volume above 300/h

The opposite failure: mail is going out, too much of it. The login throttles
cap each inbox and each IP, not the total, so this aggregate ceiling is the
only signal for volume spread across many addresses and IPs.

- [ ] Break the volume down: `sum by (template) (rate(stellarindex_notify_sends_total{result="sent"}[15m])) * 3600`.
- [ ] `magic-link` dominating: look for many `/v1/auth/login` requests from
  many IPs in the API log; tighten the edge rate limit on that route if the
  pattern is abusive.
- [ ] `signup-verify` / other template dominating with no matching request
  volume: suspect a send loop in our code; roll back the recent change.
- [ ] A genuine traffic spike (launch, press) is a valid cause: silence for
  its duration rather than raising the ceiling.

### Changelog

- 2026-08-25 — initial draft alongside the task #33 / W8 recon 9c notify counter.

## Related

- `configs/healthchecks/README.md`: per-binary heartbeat timers (also Healthchecks.io); the deadman says "alerting pipeline alive", the per-binary checks say which service died.
- The Healthchecks.io status page (bookmark it).
- Per-service runbooks if a service is down rather than unscrapeable.
- F-0085 (audit-2026-05-26) and the 2026-05-10 SEV-2 postmortem: origin of the exporter-down family.

**`stellarindex_metrics_registry_absent`**

- `ingestion.md#stellarindex_ledgerstream_tier_both_missing` — the P1 page that is now LIVE
  regardless of this gauge (W5-mon-3), no longer gated on it.
- `internal/pipeline/datastore.go` — `LedgerstreamConfig`, the builder
  that leaves `Registry` nil (affects only the SDK buffer metrics now).
- `internal/ledgerstream/tiered.go` — the `TieredDataStore`, whose tier
  metrics are sourced from `internal/obs` (always registered).

**`stellarindex_healthcheck_ping_undelivered`**

- [Alerts catalogue](../alerts-catalog.md)

**`stellarindex_notify_send_failure_ratio_high`**

- `internal/notify` — the Resend client and its `ErrProviderRejected` /
  `ErrTransient` / `ErrInvalidMessage` error classes.
- The dashboard-auth login flow (`internal/api/v1/dashboardauth`) and the
  API-signup verify flow (`cmd/stellarindex-api`) — the two send call sites.
