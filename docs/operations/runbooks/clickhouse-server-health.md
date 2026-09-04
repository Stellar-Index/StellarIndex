---
title: Runbook — ClickHouse server health
last_verified: 2026-09-04
status: draft
severity: P1
---

# Runbook — `stellarindex_clickhouse_server_down` / `_query_failures_high` / `_inserts_rejected`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_clickhouse_server_down` (scrape target down 2 min, **or** `up{job="clickhouse"}` absent for 10 min), `stellarindex_clickhouse_query_failures_high` (> 0.1 failed queries/s for 15 min), `stellarindex_clickhouse_inserts_rejected` (any "too many parts" rejection in the trailing hour — fires 5 min after the first, holds until the hour is clear) |
| Severity | P1 (page) for `_server_down`; P3 (ticket) for the other two |
| Detected by | `deploy/monitoring/rules/clickhouse.yml` + `configs/prometheus/rules.r1/clickhouse.yml` |
| Producer | **clickhouse-server itself** on port 9363 — there is no exporter process. Enabled by the archival-node role's `22-clickhouse-exporter.yml` drop-in; scraped by the `clickhouse` job in `configs/prometheus/prometheus.r1.yml` |
| Typical MTTR | 10 min (`_server_down` where the config is simply not applied), 30 min otherwise |
| Impact | ClickHouse is the lake (ADR-0034). The API's explorer reads (`/v1/ledgers`, `/v1/tx`, `/v1/accounts/*`), the supply/census rollups and the CH-fed projector all read it. While `_server_down` holds, none of that has a health signal at all. |

## Why this exists

The stock `/etc/clickhouse-server/config.xml` ships its entire
`<prometheus>` block **inside an XML comment**. Measured on r1
(2026-09-03): lines 1151–1158 commented, nothing listening on 9363,
`curl http://127.0.0.1:9363/metrics` refused, and Prometheus holding
**zero** `ClickHouse*` series. The lake's only coverage in the whole
alert tree was ingest-side — live-sink drop counters the project's own
binaries emit — so the API's ClickHouse READ path had no signal whatsoever. A
ClickHouse failing served queries, or shedding inserts under merge
back-pressure, produced no alert while it happened and left no series
to read afterwards.

Because the endpoint is inside the server, `up{job="clickhouse"} == 0`
is not "an exporter crashed" — it is normally the server. The absent
arm covers the other half: with no scrape job at all the series does
not exist, `== 0` evaluates over an empty vector, and the alert could
never fire (the F-0085 / OBS-2 blindness pattern).

## Metric namespace

ClickHouse exports its own names, not ours:

| Prefix | Source table | Used by |
| ------ | ------------ | ------- |
| `ClickHouseProfileEvents_*` | `system.events` (monotonic counters) | `FailedQuery`, `RejectedInserts` |
| `ClickHouseMetrics_*` | `system.metrics` (instantaneous gauges) | ad-hoc triage |
| `ClickHouseAsyncMetrics_*` | `system.asynchronous_metrics` | ad-hoc triage |

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Is the endpoint there at all? (000/connection refused = not enabled
#    or server down; 200 = enabled and answering)
curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 http://127.0.0.1:9363/metrics

# 2. Enabled? The role's drop-in is the only carrier of the setting
#    (`<prometheus>` is a config-file section, not a system.server_settings
#    row — a SELECT there says nothing). The listener is the proof.
ls -l /etc/clickhouse-server/config.d/si-prometheus.xml
ss -ltnp 'sport = :9363'

# 3. Is the server itself healthy?
systemctl status clickhouse-server
journalctl -u clickhouse-server -n 50 --no-pager

# 4. For _query_failures_high — what is failing, and why?
clickhouse-client --port 9300 -q "SELECT type, exception_code, count() \
  FROM system.query_log WHERE event_time > now() - INTERVAL 1 HOUR \
  AND type != 'QueryFinish' GROUP BY 1, 2 ORDER BY 3 DESC FORMAT PrettyCompact"

# 5. For _inserts_rejected — which partition has too many parts?
clickhouse-client --port 9300 -q "SELECT database, table, partition, count() \
  FROM system.parts WHERE active GROUP BY 1, 2, 3 ORDER BY 4 DESC LIMIT 20 \
  FORMAT PrettyCompact"
clickhouse-client --port 9300 -q "SELECT * FROM system.merges FORMAT Vertical"
```

## Mitigation (≤ 15 min)

### A. `_server_down` and the endpoint was never enabled

This is the expected first firing: the rules ship with `deploy.yml`,
the ansible drop-in does not. From `configs/ansible`:

- [ ] `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter --check --diff`
- [ ] `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags clickhouse-exporter`
      (tag-limited runs need `-e ansible_python_interpreter=/usr/bin/python3`)
- [ ] Verification: the play's own `Verify the ClickHouse Prometheus
      endpoint is serving` task must pass — it polls
      `http://127.0.0.1:9363/metrics` for a `ClickHouseProfileEvents_`
      line. No restart of clickhouse-server is needed or performed; the
      `config.d` reloader picks the drop-in up within seconds.
- [ ] The alert clears within ~2 min of the first successful scrape.

If Prometheus has no `clickhouse` job (`curl -s
http://127.0.0.1:9090/api/v1/targets | grep clickhouse` empty), ship
`configs/prometheus/prometheus.r1.yml` and `systemctl reload
prometheus` as well.

### B. `_server_down` and clickhouse-server is actually down

- [ ] `systemctl start clickhouse-server`; read the journal for the
      refusal (a bad `config.d` drop-in, a full pool, a corrupt part).
- [ ] Expect ingest gaps for the outage window. `ch-live-catchup.timer`
      is the only thing that heals a Tier-1 hole — confirm it runs and
      that `ContiguousWatermark` resumes climbing, or the projector
      stays clamped (see [projector-lag](projector-lag.md)).

### C. `_query_failures_high`

- [ ] Read the exception class from step 4 before changing anything.
      `READONLY` points at the ADR-0048 D4 serving profile
      (`20-clickhouse-serving-profile.yml`); `MEMORY_LIMIT_EXCEEDED` at
      a heavy job competing with served reads; `UNKNOWN_TABLE` /
      `TYPE_MISMATCH` at a schema change that landed on one side only —
      cross-check `ch-schema-drift.service`.
- [ ] Do **not** widen the serving profile's limits to silence this.
      The profile is a bound on public traffic, not a budget to grow.

### D. `_inserts_rejected`

- [ ] The alert holds for an hour after the last rejection, so a burst
      that has already stopped is still on the board — read it as "this
      happened", not "this is happening". `increase(ClickHouseProfileEvents_RejectedInserts[1h])`
      is the count; `rate(...[5m]) > 0` says whether it is still going.
- [ ] Find the partition (step 5) and confirm whether merges are
      running, starved or stuck.
- [ ] A rejected block is a GAP: the in-dispatcher dual-sink does not
      retry it. After the back-pressure clears, verify
      `ch-live-catchup.timer` has healed the range.

## Known false-positive patterns

- **`_server_down` during a deliberate ClickHouse restart** (a version
  upgrade, an `si-*.xml` change that genuinely needs one). Silence for
  the window rather than removing the alert.
- **`_inserts_rejected` during a bulk backfill.** `ch-backfill` writes
  far faster than live ingest; a burst of rejections while a certified
  range is being filled is back-pressure working, not data loss, **as
  long as the backfill's own resume state records the window as
  incomplete**. Verify that before dismissing it.

## Related

- `configs/ansible/roles/archival-node/tasks/22-clickhouse-exporter.yml`
  — the drop-in that enables the endpoint;
  `templates/clickhouse-prometheus.xml.j2` is the rendered file.
- [ch-schema-restore](ch-schema-restore.md) — the lake's schema+state
  backup, and the other ClickHouse alert family.
- [exporter-down](exporter-down.md) — the same blindness pattern for
  the exporters that DO run as separate processes.
- [projector-lag](projector-lag.md) — what an unhealed lake gap does to
  the CH-fed projector.
- ADR-0034 (ClickHouse raw lake / Postgres served tier), ADR-0048 D4
  (the serving-query settings profile).

## Changelog

- 2026-09-04 — initial version. Before it, ClickHouse exported no
  metrics at all: the stock `<prometheus>` block is commented out, so
  the lake behind every explorer read had no scrape target and no rule.
  `_inserts_rejected` reads the trailing hour with a 5 min `for`, so a
  single rejection and a five-minute burst both fire and hold for the
  hour; both shapes are pinned in
  `deploy/monitoring/rule-tests/clickhouse_test.yml`. Step 2 checks the
  listener rather than querying `system.server_settings`.
