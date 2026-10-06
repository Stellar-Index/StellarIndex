---
title: Runbook — ClickHouse alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — ClickHouse server health

Rules: `deploy/monitoring/rules/clickhouse.yml` and `configs/prometheus/rules.r1/clickhouse.yml` (identical). Severity: P1 (page) for `_server_down`; P3 (ticket) for the other two. Typical MTTR: 10 min (`_server_down` where the config is simply not applied), 30 min otherwise.

ClickHouse is the lake (ADR-0034). The API's explorer reads (`/v1/ledgers`, `/v1/tx`, `/v1/accounts/*`), the supply/census rollups and the CH-fed projector all read it. While `_server_down` holds, none of that has a health signal.

**Producer:** clickhouse-server itself on port 9363; there is no exporter process. Enabled by the archival-node role's `configs/ansible/roles/archival-node/tasks/22-clickhouse-exporter.yml` drop-in (`templates/clickhouse-prometheus.xml.j2` renders `/etc/clickhouse-server/config.d/si-prometheus.xml`); scraped by the `clickhouse` job in `configs/prometheus/prometheus.r1.yml`. The stock `/etc/clickhouse-server/config.xml` ships its `<prometheus>` block inside an XML comment, so without the drop-in nothing listens on 9363.

**Metric namespace** (ClickHouse's own names):

| Prefix | Source table | Used by |
| ------ | ------------ | ------- |
| `ClickHouseProfileEvents_*` | `system.events` (counters) | `FailedQuery`, `RejectedInserts` |
| `ClickHouseMetrics_*` | `system.metrics` (gauges) | ad-hoc triage |
| `ClickHouseAsyncMetrics_*` | `system.asynchronous_metrics` | ad-hoc triage |

## Quick diagnosis

```sh
ssh root@136.243.90.96

# 1. Endpoint there? (000/refused = not enabled or server down; 200 = answering)
curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 http://127.0.0.1:9363/metrics

# 2. Enabled? <prometheus> is a config-file section, not a system.server_settings
#    row, so the listener is the proof.
ls -l /etc/clickhouse-server/config.d/si-prometheus.xml
ss -ltnp 'sport = :9363'

# 3. Server healthy?
systemctl status clickhouse-server
journalctl -u clickhouse-server -n 50 --no-pager

# 4. _query_failures_high: what is failing?
clickhouse-client --port 9300 -q "SELECT type, exception_code, count() \
  FROM system.query_log WHERE event_time > now() - INTERVAL 1 HOUR \
  AND type != 'QueryFinish' GROUP BY 1, 2 ORDER BY 3 DESC FORMAT PrettyCompact"

# 5. _inserts_rejected: which partition has too many parts?
clickhouse-client --port 9300 -q "SELECT database, table, partition, count() \
  FROM system.parts WHERE active GROUP BY 1, 2, 3 ORDER BY 4 DESC LIMIT 20 \
  FORMAT PrettyCompact"
clickhouse-client --port 9300 -q "SELECT * FROM system.merges FORMAT Vertical"
```

## At a glance

- [`stellarindex_clickhouse_server_down`](#stellarindex_clickhouse_server_down)
- [`stellarindex_clickhouse_query_failures_high`](#stellarindex_clickhouse_query_failures_high)
- [`stellarindex_clickhouse_inserts_rejected`](#stellarindex_clickhouse_inserts_rejected)

## stellarindex_clickhouse_server_down

Trips: `up{job="clickhouse"} == 0 OR absent_over_time(up{job="clickhouse"}[10m]) == 1`, `for: 2m`, severity page.

The scrape target is down 2+ min, or no `up{job="clickhouse"}` series exists. The endpoint is inside the server, so a down target is normally the SERVER being down. The absent arm covers a missing scrape job or never-enabled endpoint (`== 0` over an empty vector can never fire). This alert is expected to fire from the moment the rules deploy until the drop-in is applied; that firing is the finding. While it holds, every other ClickHouse rule is blind.

**A. Endpoint never enabled** (rules ship with `deploy.yml`, the ansible drop-in does not). From `configs/ansible`:

- [ ] `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter --check --diff`
- [ ] `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter` (tag-limited runs need `-e ansible_python_interpreter=/usr/bin/python3`)
- [ ] The play's `Verify the ClickHouse Prometheus endpoint is serving` task must pass: it polls `http://127.0.0.1:9363/metrics` for a `ClickHouseProfileEvents_` line. No clickhouse-server restart is needed or performed; the `config.d` reloader picks the drop-in up within seconds.
- [ ] The alert clears within ~2 min of the first successful scrape.

If Prometheus has no `clickhouse` job (`curl -s http://127.0.0.1:9090/api/v1/targets | grep clickhouse` empty), ship `configs/prometheus/prometheus.r1.yml` and `systemctl reload prometheus` as well.

**B. Server actually down**

- [ ] `systemctl start clickhouse-server`; read the journal for the refusal (bad `config.d` drop-in, full pool, corrupt part).
- [ ] Expect ingest gaps for the outage window. `ch-live-catchup.timer` is the only thing that heals a Tier-1 hole; confirm it runs and that `ContiguousWatermark` resumes climbing, or the projector stays clamped ([projector-lag](projector.md#stellarindex_projector_lag_high)).

False positive: a deliberate ClickHouse restart (version upgrade, an `si-*.xml` change that needs one). Silence for the window rather than removing the alert.

## stellarindex_clickhouse_query_failures_high

Trips: `rate(ClickHouseProfileEvents_FailedQuery[10m]) > 0.1`, `for: 15m`, severity ticket.

More than 0.1 failed queries/s (about 6/min) for 15 min; each is a served read, rollup or ops job that got an exception. The API's ClickHouse-backed explorer routes surface it as a 5xx (the serving reader has an 8 s ceiling).

- [ ] Read the exception class (step 4) before changing anything. `READONLY` points at the ADR-0048 D4 serving profile (`20-clickhouse-serving-profile.yml`); `MEMORY_LIMIT_EXCEEDED` at a heavy job competing with served reads; `UNKNOWN_TABLE` / `TYPE_MISMATCH` at a schema change that landed on one side only (cross-check `ch-schema-drift.service`).
- [ ] Do NOT widen the serving profile's limits to silence this: the profile bounds public traffic, it is not a budget to grow.

## stellarindex_clickhouse_inserts_rejected

Trips: `increase(ClickHouseProfileEvents_RejectedInserts[1h]) > 0`, `for: 5m`, severity ticket.

An INSERT was refused with "Too many parts": merges are not keeping up on some partition. Fires 5 min after the first rejection and holds until the trailing hour is clear (the short `for` is a scrape-flap debounce), so a burst that has already stopped is still on the board: read it as "this happened". `rate(ClickHouseProfileEvents_RejectedInserts[5m]) > 0` says whether it is still going. A rejected block is a GAP: the in-dispatcher dual-sink does not retry it, and the projector clamps to the contiguous watermark, so an unhealed gap stalls it. Burst shapes are pinned in `deploy/monitoring/rule-tests/clickhouse_test.yml`.

- [ ] Find the partition (step 5); confirm whether merges are running, starved or stuck (`system.merges`, `system.mutations`).
- [ ] After back-pressure clears, verify `ch-live-catchup.timer` has healed the range.

False positive: during a bulk `ch-backfill` (writes far faster than live ingest), a burst of rejections is back-pressure working, not data loss, as long as the backfill's own resume state records the window as incomplete. Verify that before dismissing.

## Related

- [ch-schema-restore](ch-schema-restore.md): the lake's schema+state backup, and the other ClickHouse alert family.
- [exporter-down](meta.md#stellarindex_redis_exporter_down): the same blindness pattern for exporters that run as separate processes.
- [projector-lag](projector.md#stellarindex_projector_lag_high): what an unhealed lake gap does to the CH-fed projector.
- ADR-0034 (raw lake / served tier), ADR-0048 D4 (serving-query settings profile).
