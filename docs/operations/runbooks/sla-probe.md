---
title: Runbook — sla-probe
last_verified: 2026-08-28
status: living
severity: P2
---

# Runbook — SLA probe alerts

Six alerts, group `stellarindex.sla_probe`, rules `configs/prometheus/rules.r1/sla-probe.yml` (multi-host twin `deploy/monitoring/rules/sla-probe.yml`). Five are `severity: page` (P2); `stellarindex_sla_probe_unit_failed_alert` is `ticket` (P3). All `for: 30m` except `_stale` (`5m`).

The probe (`cmd/stellarindex-sla-probe`) runs from `stellarindex-sla-probe.timer` (15 min) via `configs/healthchecks/sla-probe.sh`, writes `/var/lib/node_exporter/textfile_collector/sla_probe.prom` and POSTs the JSON report to Healthchecks.io. The JSON never reaches journald: read the textfile, the check's last ping body, or run it once.

Shared commands (on r1, `ssh root@136.243.90.96`):

```sh
# Last run's verdict
cat /var/lib/node_exporter/textfile_collector/sla_probe.prom

# One-off run; prints failed_reasons
export STELLARINDEX_PROBE_API_KEY=<key>   # or pass -api-key
/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 \
  -pair native,fiat:USD -duration 30s -concurrency 1 -report-format json | jq .failed_reasons
```

Probe gotchas that fake a breach: keep `-concurrency 1` (at 2 it self-saturates, F-1305); authenticate (a keyless run hits the anonymous 60/min limit and 429s read as an availability failure, F-1311); a deploy restart or cold cache can trip one run (the 30 m `for` absorbs it); probe host CPU contention.

Published targets (`/sla`): p95 <= 200 ms, p99 <= 500 ms, availability >= 99.9 % (2xx share), freshness 30 s (150 s for `/v1/price`).

## At a glance

Six probe alerts (P2 page, `_unit_failed_alert` P3 ticket) plus two api-smoke ticket alerts; probe runs every 15 min on r1.

## stellarindex_sla_probe_p95_breach

**Symptom.** `stellarindex_sla_probe_latency_ms{endpoint=...,quantile="0.95"} > 200` for 30 min (page); `failed_reasons` carries `<endpoint>: p95=<N>ms > target 200.0ms`. `stellarindex_api_latency_p95_high` (real traffic) may also fire.

**Check.**

```sh
# 1. Verdict: see shared commands above.

# 2. Do direct-traffic histograms agree? (rules out probe-only artefacts)
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.95, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~"stellarindex[_-]api"}[5m])))' | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head

# 3. One-off probe from another network to see if it is regional.
# Keep -concurrency 1 and authenticate: a keyless run hits the anonymous
# 60/min limit and 429s read as an availability failure (F-1311).
export STELLARINDEX_PROBE_API_KEY=<key>   # or pass -api-key
stellarindex-sla-probe -base-url https://api.stellarindex.io/v1 \
  -duration 10s -concurrency 1 -report-format text
```

**Likely causes.**

- Same backend causes as `api.md#stellarindex_api_latency_p95_high`.
- Probe-host network path: probe slow, histogram fine.
- One endpoint slow (e.g. `/v1/oracle/latest` SEP-40 read).
- Cold buffers after a deploy.

**Fix.**

1. Histogram agrees: route to `api.md#stellarindex_api_latency_p95_high`.
2. Probe-only: investigate the probe host's network; not a real-impact alert.
3. Verify probe p95 under 200 ms for 30 min (2 passes).

**Escalate:** page per [sev-playbook](../sev-playbook.md).

## stellarindex_sla_probe_p99_breach

**Symptom.** `stellarindex_sla_probe_latency_ms{endpoint=...,quantile="0.99"} > 500` for 30 min (page). The p95 twin: with p95 healthy the tail is usually one slow path or heavy pair. `stellarindex_api_latency_p99_high` (> 2 s) is a fleet backstop, not the SLA line.

**Check.** As [p95](#stellarindex_sla_probe_p95_breach) step 2 with the 0.99 quantile:

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.99, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~"stellarindex[_-]api"}[5m])))' | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head
```

**Fix.** Confirm real-traffic p99 agrees; route to `api.md#stellarindex_api_latency_p95_high`; verify probe p99 under 500 ms for 30 min. **Escalate:** as p95.

## stellarindex_sla_probe_availability_breach

**Symptom.** `stellarindex_sla_probe_availability_pct{endpoint=...} < 99.9` for 30 min (page). A run holds ~150 samples per endpoint, so one non-2xx dips below 99.9 %; the probe counts every 429, 5xx and client timeout. `stellarindex_sla_probe_unit_failed_alert` follows as the umbrella.

**Check.** Verdict and one-off run per shared commands, then:

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=sum by (status) (rate(http_requests_total{job=~"stellarindex[_-]api"}[15m]))' | \
  jq -r '.data.result[] | "\(.metric.status): \(.value[1])"'
```

**Likely causes.** Probe-side 429s (missing, revoked or under-quota key; API healthy); real 5xx; timeouts; a deploy restart inside one run.

**Fix.**

1. 429 on the probe only: fix the key, not the API.
2. 5xx: `api.md#stellarindex_api_error_rate_critical`.
3. Timeouts: `api.md#stellarindex_api_latency_p95_high`.
4. Verify two consecutive runs at >= 99.9 %.

**Escalate:** page per [sev-playbook](../sev-playbook.md).

## stellarindex_sla_probe_freshness_breach

**Symptom.** Page, `for: 30m`:

```promql
stellarindex_sla_probe_freshness_sec{endpoint="price"} > 150
or
stellarindex_sla_probe_freshness_sec{endpoint!="price"} > 30
```

Read the alert `summary` for the value. `/v1/price` serves the last CLOSED bucket (ADR-0015), so its `observed_at` is 30-150 s old by design (`defaultClosedBucketFreshTarget`, `cmd/stellarindex-sla-probe/main.go`); `price-tip` is held to 30 s. `oracle-latest` emits no `observed_at`. Affected pairs get `flags.stale: true`.

**Check.** Bisect the three-stage chain:

```sh
# 1. Probe's view of price (one-off run), and last run's exported freshness
ssh root@136.243.90.96 '/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 -pair native,fiat:USD -report-format json' \
  | jq '.per_endpoint[] | select(.endpoint=="price")'
ssh root@136.243.90.96 'grep freshness /var/lib/node_exporter/textfile_collector/sla_probe.prom'

# 2. Direct API check on the same pair
curl -s 'https://api.stellarindex.io/v1/price?asset=native&quote=fiat:USD' | jq

# 3. Redis cache keys. F-1309: aggregator writes vwap:<base>:<quote>:<window-seconds>,
#    not a single price:<base>:<quote> key. TTLs differ (300s = 5m, 3600s = 1h, 86400s = 1d).
ssh root@136.243.90.96 "redis-cli GET 'vwap:native:fiat:USD:300'"
ssh root@136.243.90.96 "redis-cli GET 'vwap:native:fiat:USD:3600'"
ssh root@136.243.90.96 "redis-cli GET 'vwap:native:fiat:USD:86400'"

# 4. Latest closed bucket in Postgres. F-1309: column is `bucket`, NOT `ts` (that is trades).
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "
  SELECT bucket, vwap FROM prices_1m
  WHERE base_asset='"'"'native'"'"' AND quote_asset='"'"'fiat:USD'"'"'
  ORDER BY bucket DESC LIMIT 5;"'
```

Postgres fresh but Redis stale: aggregator is not writing the cache. Postgres stale too: ingestion. Both fresh but API stale: wrong key.

**Likely causes.**

- Aggregator wedged: `rate(stellarindex_aggregator_ticks_total[5m]) == 0` ([aggregator-silent](aggregator.md#stellarindex_aggregator_silent)). Restart it.
- Indexer lag: [source-stopped](ingestion.md#stellarindex_ingestion_source_stopped); `stellar-node.md#stellarindex_stellar_core_ledger_age`.
- CAGG refresh paused: `timescale.md#stellarindex_timescale_cagg_stale`.
- No trades for the pair in the tip window (common on `price-tip`): `computeTip` escalates 5 s to 30 s then falls back to the closed bucket (ADR-0018); the tell is `price` and `price-tip` reporting identical freshness. Check the trade gap:

  ```sh
  ssh root@136.243.90.96 "cd /tmp && runuser -u postgres -- psql -d stellarindex -X -c \"
    SELECT ts, source, quote_asset FROM trades
    WHERE ts >= now() - interval '5 minutes'
      AND base_asset IN ('native','crypto:XLM')
      AND quote_asset = 'fiat:USD'
    ORDER BY ts;\""
  ```

  A quiet pair needs source coverage, not a looser bound: ack, do not widen 30 s.
- A binary predating the 2026-09-05 fix measures freshness at end of run; subtract `run_duration_seconds / 2` from the reading.

**Fix.**

1. Bisect to the lagging stage and follow that runbook.
2. Honest "no trades" staleness: confirm the pair is quiet and ack.
3. Pre-silence planned indexer/aggregator pauses.
4. Verify `freshness_sec` under its bound (150 s `price`, 30 s others) for 30 min.

**Escalate:** page per [sev-playbook](../sev-playbook.md).

## stellarindex_sla_probe_unit_failed_alert

**Symptom.** `stellarindex_sla_probe_unit_failed > 0` for 30 min (ticket, P3). Umbrella: `failed_reasons` is non-empty, usually beside a per-breach alert ([p95](#stellarindex_sla_probe_p95_breach), [p99](#stellarindex_sla_probe_p99_breach), [availability](#stellarindex_sla_probe_availability_breach), [freshness](#stellarindex_sla_probe_freshness_breach)). With no companion, a threshold lacks its own rule: add one.

**Check.** Reproduce the verdict on r1:

```sh
ssh root@136.243.90.96
set -a; source /etc/default/stellarindex-healthchecks; set +a
/usr/local/bin/stellarindex-sla-probe -base-url "${SLA_PROBE_BASE_URL:-http://localhost:3000/v1}" \
  -pair "${SLA_PROBE_PAIR:-native,fiat:USD}" -duration 30s -concurrency 1 \
  -report-format json | jq -r '.failed_reasons[]'
```

Output is one of:

```
price: p95=215.3ms > target 200.0ms
price: p99=523.1ms > target 500.0ms
healthz: availability=98.50% < target 99.90%
price-tip: freshness=42.1s > target 30.0s
price: freshness=163.2s > target 150.0s
```

**Fix.** Route by breach kind to p95/p99, freshness or availability above; verify `unit_failed` at 0 for 30 min. A firing with no real breach is a bug in `cmd/stellarindex-sla-probe/main.go::computeVerdict`.

**Escalate:** none (ticket).

## stellarindex_sla_probe_stale

**Symptom.** Page (P2), `for: 5m`; the SLA-evidence trail is lost (the API itself may be fine):

```promql
(time() - stellarindex_sla_probe_last_pass_timestamp) > (90 * 60)
or
absent_over_time(stellarindex_sla_probe_last_pass_timestamp[90m])
```

The probe writes `last_pass_timestamp` only on a pass and rewrites the whole file each run, so all-runs-failing makes the series ABSENT, not old (also fires `_unit_failed_alert`). The live timer is `stellarindex-sla-probe.timer`; the retired GO-stack `sla-probe.timer` is removed by ansible.

**Check.**

```sh
ssh root@136.243.90.96

# 1. Is the timer scheduled?
systemctl status stellarindex-sla-probe.timer
systemctl list-timers stellarindex-sla-probe.timer

# 2. When did the unit last run?
journalctl -u stellarindex-sla-probe.service --since "2 hours ago" -n 50

# 3. Is the textfile being written?
ls -la /var/lib/node_exporter/textfile_collector/sla_probe.prom

# 4. Force a run, then read the verdict (textfile, or run the binary for JSON)
systemctl start stellarindex-sla-probe.service
cat /var/lib/node_exporter/textfile_collector/sla_probe.prom
/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 \
  -pair native,fiat:USD -duration 30s -concurrency 1 -report-format json | jq .
```

**Likely causes.**

- Timer disabled: `sudo systemctl enable --now stellarindex-sla-probe.timer`.
- Unit failing every run: handle as [unit_failed](#stellarindex_sla_probe_unit_failed_alert).
- node_exporter not scraping the textfile dir: `grep last_pass /var/lib/node_exporter/textfile_collector/sla_probe.prom`; line present but series absent means check `--collector.textfile.directory`; line absent from a fresh file means every run fails.
- `SLA_PROBE_TEXTFILE_OUTPUT=` blanked in `/etc/default/stellarindex-healthchecks` (the wrapper defaults the path, `configs/healthchecks/sla-probe.sh:60`), or `ReadWritePaths`/EROFS on the unit.

**Fix.**

1. Apply the matching fix above.
2. `systemctl start stellarindex-sla-probe.service`; confirm `last_pass_timestamp` updates.
3. The alert clears within 5 min of a passing run reaching node_exporter. After a fresh probe deploy it fires until the first passing run: force a run, do not silence.

**Escalate:** page per [sev-playbook](../sev-playbook.md).

## api-smoke

Alerts from `configs/prometheus/rules.r1/api-smoke.yml` (group `stellarindex.api_smoke`, ticket; multi-host twin `deploy/monitoring/rules/api-smoke.yml`). The smoke (`scripts/dev/r1-smoke.sh`, wrapper `configs/healthchecks/smoke.sh`, unit `stellarindex-smoke.service`) reads response bodies every 5 min and writes `/var/lib/node_exporter/textfile_collector/api_smoke.prom`. `_failing` means it ran and found a problem; `_stale` means it did not report.

### stellarindex_api_smoke_failing

**Symptom.** `stellarindex_api_smoke_failures > 0` for 30m (P3): the gauge is the failed-check count (34 checks), rewritten each run. A launch-critical endpoint answers with the wrong shape or status. The 30m `for` absorbs data-dependent checks (`/v1/ohlc`, `/v1/oracle/prices` accept a documented 404 on an empty window; cold-cache responses near the 10 s budget).

**Check.**

```sh
ssh root@136.243.90.96

# 1. Which checks are failing? Each failing check prints its own line.
journalctl -u stellarindex-smoke.service --since "1 hour ago" -n 200

# 2. Current verdict as Prometheus sees it.
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom

# 3. Reproduce on demand (read-only, safe to re-run).
API_BASE_URL=http://localhost:3000 \
  bash /opt/stellarindex/healthchecks/r1-smoke.sh; echo "failed: $?"

# 4. Same surface from outside, to separate "API is wrong" from "loopback path is wrong" (Caddy, TLS, host routing).
API_BASE_URL=https://api.stellarindex.io bash scripts/dev/r1-smoke.sh
```

**Likely causes.**

- Handler changed shape, or a documented 4xx regressed to 200 (an API contract break).
- Binary deployed without its rendered config: check `config-apply-gate` on the deploy run.
- Deployed `r1-smoke.sh` differs from the repo: `diff scripts/dev/r1-smoke.sh /opt/stellarindex/healthchecks/r1-smoke.sh`.
- Dependency down (many checks fail at once): work `api-5xx` / `api-down` first.

**Fix.**

1. Fix forward, or roll back per `docs/operations/deploy-config-apply.md` (prefer rollback during the window).
2. Ignore one slow cold-cache run (`/v1/markets?limit=5` takes 6-8 s) or a run during a deploy.
3. Re-run manually; verify `stellarindex_api_smoke_failures` returns to 0 at the next firing.

**Escalate:** none; see `sla-probe.md#stellarindex_sla_probe_unit_failed_alert`, and `meta.md#stellarindex_healthcheck_ping_undelivered` before trusting a Healthchecks.io "down" email.

### stellarindex_api_smoke_stale

**Symptom.** P3, for 5m: `(time() - stellarindex_api_smoke_last_run_unix) > 30*60 or absent_over_time(stellarindex_api_smoke_last_run_unix[30m])`. Nothing asserts the public API's shapes. Present-but-old stamp means a frozen file; absent series means never scheduled or written.

**Check.**

```sh
ssh root@136.243.90.96

# 1. Is the timer scheduled, and when did it last fire?
systemctl list-timers stellarindex-smoke.timer
systemctl status stellarindex-smoke.timer

# 2. Is the unit running, or failing before it can write?
journalctl -u stellarindex-smoke.service --since "2 hours ago" -n 100

# 3. Is the textfile there, and how old is it?
ls -la /var/lib/node_exporter/textfile_collector/api_smoke.prom
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom

# 4. Force one run and confirm the stamp advances.
systemctl start stellarindex-smoke.service
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom
```

**Likely causes.**

- Timer disabled: `sudo systemctl enable --now stellarindex-smoke.timer`.
- `ReadWritePaths` missing: `ProtectSystem=strict` makes the textfile write fail (EROFS; journal `smoke: WARN … not writable`). The unit must ship `ReadWritePaths=/var/lib/node_exporter/textfile_collector` and `SupplementaryGroups=stellarindex` (`configs/healthchecks/stellarindex-smoke.service`).
- Collector directory missing (archival-node role `10-observability.yml`), or node_exporter not reading it (`--collector.textfile.directory`).
- Bad `ExecStart` path after a partial deploy.

**Fix.**

1. Re-apply the role if needed: `ansible-playbook … archival-node.yml --tags healthchecks`.
2. `systemctl start stellarindex-smoke.service`; confirm `last_run_unix` advances. Clears ~5 min after a fresh scrape; after a fresh deploy force a run, do not silence.

**Escalate:** none; see `sla-probe.md#stellarindex_sla_probe_stale`, `data-freshness.md#stellarindex_data_freshness_watchdog_silent`.

## Related

- `api.md#stellarindex_api_latency_p95_high`, `api.md#stellarindex_api_error_rate_critical`, `slo.md#stellarindex_slo_availability_burn_fast`.
- `timescale.md#stellarindex_timescale_cagg_stale`, `stellar-node.md#stellarindex_stellar_core_ledger_age`, `aggregator.md#stellarindex_aggregator_silent`: freshness chain.
- ADR-0015: closed-bucket-only serving.
