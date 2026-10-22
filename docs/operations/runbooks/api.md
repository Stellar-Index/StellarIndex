---
title: Runbook — api alerts
last_verified: 2026-10-05
status: draft
---
# API alerts

One section per alert in `configs/prometheus/rules.r1/api.yml`. r1 is a single host: Cloudflare -> Caddy (`configs/caddy/Caddyfile.api`) -> `stellarindex-api.service` on `localhost:3000`. The multi-host HAProxy shape ([ADR-0008](../../adr/0008-ha-topology.md), [ha-plan.md](../../architecture/ha-plan.md)) is not deployed.

## At a glance

Per-alert sections below: symptom, check, causes, fix, escalate. Severity per section; page alerts follow [sev-playbook](../sev-playbook.md).

## stellarindex_admin_audit_write_failing

**Trigger.** `sum by (surface) (increase(stellarindex_admin_audit_write_failures_total[1h])) > 0`, `for: 5m`, P3. Compliance gap, not an outage: the mutation committed, the audit append is best-effort and was swallowed. Start recovery first; log retention is the deadline.

`surface` names what went unrecorded: key mint/revoke, account override, passkey events, status notice, or one of two staff/admin reads (the lost record is who looked at whose data).

**Check.**

```sh
journalctl -u stellarindex-api --since '-2h' | grep 'audit append failed'
```

**Likely causes.**

- Postgres unreachable or in recovery.
- `audit_log` disk full or statement timeout.
- Permissions regression on the table (`migrations/README.md` rule 7: object created as superuser, not the `stellarindex` app role).

**Fix.**

1. Rebuild missing rows from logs before journal retention closes. Each site logs the target id (`minted_key_id`, `account_id`, `notice_id`, `event_id`); with the request log line (actor key id, identifier, `X-Reason`) that rebuilds the entry. `staff customer lookup: audit append failed (best-effort)` carries `actor` + `account_id`; `admin account read: audit append failed (best-effort)` carries `actor_key_id` + `account_id`.
2. Fix the store, then confirm: `SELECT count(*) FROM audit_log WHERE created_at > now() - interval '1 day';`
3. Alert clears after an hour with no counted failure.

Do not roll back the mutation or make the append blocking. **Escalate:** none (P3); related [monthly_quota_fail_open](api.md#stellarindex_monthly_quota_fail_open).

## stellarindex_after_response_tasks_dropping

**Trigger.** `sum(rate(stellarindex_after_response_tasks_dropped_total[5m])) > 0`, `for: 10m`, P3. The bounded worker pool in `internal/api/v1/middleware/after_response.go` (usage counters, `TouchUsage`) is full and dropping tasks; responses are unaffected but usage counts leak.

**Check.** Compare `stellarindex_usage_units_dropped_total` and Redis/Postgres latency; check request volume for a spike.

**Likely causes.**

- Slow or wedged Redis/Postgres keeps workers busy.
- Sustained high request volume.

**Fix.**

1. Slow store: follow its recovery runbook; the pool drains on its own.
2. Sustained volume: raise `afterResponseWorkers` / `afterResponseQueueSize` in `after_response.go` (capacity decision, not an incident fix).

**Escalate:** none; related [usage_write_failing](api.md#stellarindex_usage_write_failing).

## stellarindex_api_error_rate_critical

**Trigger.** `sum(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[5m])) / sum(rate(http_requests_total{job="stellarindex-api"}[5m])) > 0.05`, `for: 2m`, P1. Rules: `configs/prometheus/rules.r1/{api,slo}.yml`. Also the landing page for `stellarindex_slo_availability_burn_{fast,medium,slow}` ([fast](slo.md#stellarindex_slo_availability_burn_fast), [medium](slo.md#stellarindex_slo_availability_burn_medium), [slow](slo.md#stellarindex_slo_availability_burn_slow)); treat `_burn_fast` as budget exhaustion. Sibling: [error_rate_high](#stellarindex_api_error_rate_high).

SLA: the availability budget (99.9 % non-5xx over 30 d) is the published target. The fast-burn trip point is 14.4 × 0.001 = 1.44 % 5xx, so a sustained 1.5 % error rate trips the burn alert before this page; treat the urgency as budget exhaustion.

**Check** (5 min).

```sh
curl -s 'http://127.0.0.1:9090/api/v1/query' \
  --data-urlencode 'query=topk(5, sum by (route, status) (rate(http_requests_total{job="stellarindex-api",status=~"5.."}[5m])))' \
  | jq .data.result

curl -sf http://localhost:3000/v1/version | jq -r .data.version
systemctl show stellarindex-api -p ActiveEnterTimestamp --value

curl -sSf https://api.stellarindex.io/v1/readyz | jq '.data'

journalctl -u stellarindex-api --since '15 min ago' --no-pager -o cat \
  | jq -r 'select(.level=="ERROR") | [.msg, .err] | @tsv' \
  | sort | uniq -c | sort -rn | head
```

One dominant route = handler bug; every route = shared dependency. `postgres.ok == false`: [timescale-primary-down](postgres.md#stellarindex_timescale_primary_down). `redis.ok == false`: [redis-master-down](cache.md#stellarindex_redis_master_down).

**Likely causes.**

- Release within the last hour.
- Handler bug or panic (nil-dereference on a new input shape).
- Dependency down (Postgres, Redis, [all-ingestion-down](ingestion.md#stellarindex_ingestion_all_sources_stopped)); auto-resolves on recovery.
- Load (viral traffic, DDoS) with `stellarindex_api_latency_p99_high` firing too.

**Fix.**

1. Recent deploy: revert. Auto-rollback may already have run, so check first.

   ```sh
   gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> \
     -f binaries=stellarindex-api
   ```

   Manual fallback ([release-process.md](../release-process.md) -> Rollback): swap back to `/usr/local/bin/stellarindex-api.prev-<previous-tag>` and restart. Verify `/v1/readyz` `status=ok` and `/v1/version`.
2. Handler bug, no deploy: gate the endpoint at Caddy (above the catch-all `reverse_proxy` in `/etc/caddy/Caddyfile`; codify in `configs/caddy/Caddyfile.api` in the fix PR and remove after the fixed deploy). A middleware panic cannot be gated; roll back.

   ```caddyfile
   handle /v1/history* {
       respond `{"type":"about:blank","title":"endpoint temporarily disabled","status":503}` 503 {
           close
       }
   }
   ```

   ```sh
   caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy
   ```

3. Dependency: follow its runbook.
4. Load: tighten a Cloudflare WAF short-TTL per-IP rule; 503-gate the heaviest non-essential paths (SSE `/v1/price/stream`, batch reads) as in step 2; promote AWS DR only if saturated and unresolved after 10 min ([dr-activation.md](dr-activation.md)).
5. Evidence ([sev-playbook](../sev-playbook.md) section 6): `journalctl -u stellarindex-api --since '1h ago' --no-pager`, `dmesg | grep -i oom`, `journalctl -u caddy --since '1h ago' --no-pager -o cat`.

False positive: the restart minute of a release (Caddy's 10s `/v1/healthz` check bounds it; the 2m window absorbs it). **Escalate:** SEV-1 per [sev-playbook](../sev-playbook.md); related [api_down](#stellarindex_api_down), [api_latency_p95_high](#stellarindex_api_latency_p95_high).

## stellarindex_api_error_rate_high

**Trigger.** Same expression as [error_rate_critical](#stellarindex_api_error_rate_critical) with `> 0.01`, `for: 2m`, P3 (ticket). Early warning; a brief deploy hiccup can trip it, but 1.44% for 1h is real budget burn.

**Check / Likely causes / Fix.** Identical to the critical section (top 5xx routes, deploy, `/v1/readyz`, ERROR-log grouping; revert, gate, dependency, shed load). Verify the rate falls well below 1%.

**Escalate:** page-level if it crosses 5% or `_burn_fast` fires.

## stellarindex_api_down

**Trigger.** `sum(up{job="stellarindex-api"}) == 0` for 60s OR `absent_over_time(up{job="stellarindex-api"}[5m]) == 1` (Prometheus no longer scrapes the API). P1, SEV-1: total outage. The API listens on loopback only (`127.0.0.1:3000`).

**Check** (5 min, on r1-01).

```sh
# Is the unit even running / parked in `failed`?
systemctl status stellarindex-api --no-pager | head -10

# Why did it stop? Last 100 log lines.
journalctl -u stellarindex-api -n 100 --no-pager

# Edge: is Caddy up, and is a maintenance-mode file left behind?
systemctl is-active caddy && ls -l /etc/caddy/MAINTENANCE_MODE
journalctl -u caddy -n 50 --no-pager

# Caddy's view of the upstream.
curl -s localhost:2019/metrics | grep caddy_reverse_proxy_upstreams_healthy

# Fired via the absent_over_time arm (no `up` series at all)? Then the
# fault is in Prometheus, not the API: the job is gone from the scrape
# config or the target never resolved.
curl -s localhost:9090/api/v1/targets | jq '.data.activeTargets[]
  | select(.labels.job == "stellarindex-api")
  | {health, lastError, scrapeUrl}'
grep -n 'stellarindex-api' /etc/prometheus/prometheus.yml

# If the unit is RUNNING but NOT serving: probe /v1/readyz directly.
# The response is an Envelope: {data:{status,uptime,checks:[{name,ok,error}]},as_of,flags}
curl -sS http://127.0.0.1:3000/v1/readyz | jq '.data.status, .data.checks'
```

`/v1/readyz` (`internal/api/v1/server.go::handleReadyz`): `postgres`, `schema`, `nonstandard_decimals` (see [`dex.md#dex-nonstandard-decimals`](dex.md#dex-nonstandard-decimals)) and `closed_buckets` (see [`dependency-down.md`](dependency-down.md#if-dependencyclosed_buckets)) return 503 when red; `schema-dirty`, `redis` and `clickhouse` only degrade (200). Caddy probes only `/v1/healthz`, so a readyz 503 does not drop the upstream. `postgres` red: [`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down). `schema` red: compare `cat /var/lib/stellarindex/deployed-versions/stellarindex-api` and `curl -sf http://127.0.0.1:3000/v1/version` with the applied migration head; run `stellarindex-migrate` for the deployed tag (or roll back) before restarting.

**Likely causes.**

- Bad release: the binary fails `config.Validate()` and exits; systemd parks the unit `failed` after `StartLimitBurst=10` in 5 min.
- Credential or config rotation without a restart (`/etc/stellarindex.toml`, `/etc/default/stellarindex`).
- Stale `/etc/caddy/MAINTENANCE_MODE`, Caddy, Cloudflare or DNS: customers are down while `up==1` and this does NOT fire.
- Whole host down: expect `stellarindex_deadmansswitch` instead.

**Fix** (15 min).

1. Declare SEV-1. Find the cause before restarting: restarting a crashing binary burns the StartLimit budget. After a crash loop run `systemctl reset-failed stellarindex-api` first.
2. Release is the cause: roll back ([release-process.md](../release-process.md) -> Rollback).

   ```sh
   gh workflow run deploy.yml -f region=r1 -f version=<prev-tag> -f binaries=stellarindex-api
   ```

   Manual fallback (a brief full outage; single instance):

   ```sh
   systemctl stop stellarindex-api && cp /usr/local/bin/stellarindex-api.prev-<prev-tag> /usr/local/bin/stellarindex-api && echo <prev-tag> > /var/lib/stellarindex/deployed-versions/stellarindex-api && systemctl start stellarindex-api
   ```
3. Config or secret cause: fix the source (`configs/` or vault), run the ansible playbook, `systemctl restart stellarindex-api`.
4. Repair will take a while: `touch /etc/caddy/MAINTENANCE_MODE` serves a clean 503 and keeps `/v1/healthz` and `/metrics` alive. `rm` it afterwards; a forgotten file is an outage no alert catches.
5. Edge cause: `systemctl status caddy`, `journalctl -u caddy`, `caddy validate --config /etc/caddy/Caddyfile`, confirm in Cloudflare that `api.stellarindex.io` proxies to r1. Verify: `up{job="stellarindex-api"}` is 1 and `curl -sI https://api.stellarindex.io/v1/healthz` returns 200 from outside.

False positive: a restart over 60 s pages; a broken scrape path looks like an outage (discriminate with `stellarindex_prometheus_scrape_failing`, `stellarindex_sla_probe_availability_pct`, `stellarindex_sla_probe_unit_failed`). See [`binary-version-skew.md`](binary-version-skew.md). **Escalate:** SEV-1 per [sev-playbook](../sev-playbook.md); related [error_rate_critical](#stellarindex_api_error_rate_critical), [`cache.md#stellarindex_redis_master_down`](cache.md#stellarindex_redis_master_down).

## stellarindex_api_latency_p95_high

**Trigger.** `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m]))) > 0.5`, `for: 10m`, ticket. Rule: `configs/prometheus/rules.r1/api.yml`. SLA is p95 <= 200 ms, p99 <= 500 ms. Also the landing page for `stellarindex_slo_latency_burn_{fast,medium,slow}` ([fast](slo.md#stellarindex_slo_latency_burn_fast), [medium](slo.md#stellarindex_slo_latency_burn_medium), [slow](slo.md#stellarindex_slo_latency_burn_slow)); a `_burn_fast` page needs a real fix. Sibling [p99_high](#stellarindex_api_latency_p99_high) (p99 > 2 s, same steps).

**Check** (5 min).

```sh
# Which endpoint is slow? (Prometheus listens on localhost:9090 on r1)
ssh root@136.243.90.96 "curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.95, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~\"stellarindex[_-]api\"}[5m])))'" | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head

# Is Redis healthy? Cache-miss storms are the common trigger.
# (r1's Redis is local — there is no `redis` hostname.)
ssh root@136.243.90.96 'redis-cli --latency-history'  # in another pane
# The API serves /metrics on its public listener (:3000) — :9464 is
# the INDEXER's metrics port. The relevant cache metric is the
# in-memory API cache counter:
ssh root@136.243.90.96 "curl -s http://localhost:3000/metrics | grep 'stellarindex_api_cache_ops_total.*miss'"

# Is Timescale the bottleneck?
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "
  SELECT state, count(*), max(now()-query_start) AS oldest
  FROM pg_stat_activity WHERE application_name LIKE '"'"'stellarindex%'"'"'
  GROUP BY state;"'
```

**Likely causes.** Redis cache-miss storm (`stellarindex_api_cache_ops_total{result="miss"}` jumps; see [cache_miss_rate_high](#stellarindex_api_cache_miss_rate_high), `cache.md#stellarindex_redis_memory_saturated`); Timescale contention (a `pg_stat_activity` query over 30 s; `SELECT pg_cancel_backend(pid)` once it is confirmed not production traffic); stale CAGG (`timescale.md#stellarindex_timescale_cagg_stale`); host CPU/IO or GC pressure.

**Fix.** Narrow to the slow endpoint, then walk the causes. Timescale-driven: `postgres.md#stellarindex_timescale_connections_saturated`. Scale the API only if CPU-bound. Verify p95 under 200 ms for 15 min.

False positives: cold buffers after a deploy and the `/v1/markets` cold scan during a heavy backfill (about 540 ms cold, per-route carve-out p95 <= 300 ms). **Escalate:** none (ticket); related [error_rate_critical](#stellarindex_api_error_rate_critical).

## stellarindex_api_latency_p99_high

**Trigger.** p99 over 2 s, `for: 10m`, ticket. Tail-only: a few slow routes or a periodic heavy query is likelier than a global slowdown. **Check / Likely causes / Fix:** as [p95_high](#stellarindex_api_latency_p95_high). **Escalate:** none.

## stellarindex_api_cache_miss_rate_high

**Trigger.** Per `(cache, op)`: `sum by (cache, op) (rate(stellarindex_api_cache_ops_total{result="miss"}[5m])) / sum by (cache, op) (rate(stellarindex_api_cache_ops_total{result=~"hit|miss|stale"}[5m])) > 0.5`, read-rate floor `> 0.1 req/s`, `for: 10m`, P2. `evicted` and `refresh_error` are side-events and excluded from the denominator. An in-memory cache serves over half its requests cold (prewarm-key drift or a load spike), so that surface pays the 5-10 s SQL scan.

**Check.**

1. Read the `{cache, op}` labels. In-memory caches: `coins`, `issuers`, `markets`, `network_stats`, `observations`, `oracle`, `sources_stats` (`grep APICacheOpsTotal.WithLabelValues` under `internal/api/v1/`). The Redis read-through layers (`assets_redis`, `markets_redis`, `oracle_redis`, `prices_redis`) miss by design and are excluded. `markets` ops: `distinct_pairs` (`/v1/markets`), `source_markets` (`?source=`), `asset_markets` (`?asset=`), `all_pools` (`/v1/pools`).
2. Prewarm is `prewarmCaches` in `cmd/stellarindex-api/main.go` (`prewarmHeavy` 5 min for `sources_stats`, `prewarmLight` 60 s for the rest). Compare every argument with the handler in `internal/api/v1/markets.go`; cache keys are built in `internal/api/v1/markets_cache.go` (`fetchPairs` / `fetchPools`); known drift: Order, Sources, Limit.
3. TTL vs cadence: `v1.NewCachedMarketsReader(...)` uses `2*time.Minute`; a cadence above the TTL looks like a miss storm.
4. Key churn: `observations` and `oracle` are capped (4096 entries); a non-zero `rate(stellarindex_api_cache_ops_total{cache="<alerted>",result="evicted"}[5m])` means churn, not drift: raise the cap or rate-limit the caller.

**Likely causes.** Prewarm arguments drifted from the handler; TTL below cadence; key churn. `asset_markets` is not prewarmed, so misses there are expected.

**Fix.**

1. Fix the drifted dimension; share the derivation (e.g. `v1.DexSourceNames()`) between prewarm and handler.
2. Cut a release and deploy.
3. Verify the miss ratio is under 0.1 within one cycle (60 s light tier, 5 min `sources_stats`).

**Escalate:** none; related [p95_high](#stellarindex_api_latency_p95_high), [metrics reference](../../reference/metrics/README.md).

## stellarindex_api_cache_refresh_failing

**Trigger.** `sum by (cache, op) (increase(stellarindex_api_cache_ops_total{result="refresh_error"}[15m])) > 0`, `for: 30m`, ticket. Failed background stale-while-revalidate refreshes; the cache keeps serving the last good value with `flags.stale`.

**Check.** `{cache, op}` names the wrapper (list under [cache_miss_rate_high](#stellarindex_api_cache_miss_rate_high)).

```sh
journalctl -u stellarindex-api --since -1h | grep 'cache refresh failed'
```

Each line carries `cache`, `op`, `key` and `err`.

**Likely causes.** Upstream read failing (Postgres/ClickHouse timeout, missing relation after a migration, statement cancel).

**Fix.** Fix the upstream read, not the cache; the next good refresh clears the alert. **Escalate:** none.

## stellarindex_api_price_stale

**Trigger.** `stellarindex_price_staleness_seconds{asset,quote} > 120` for 5m, OR the series absent for 10m (`absent_over_time(stellarindex_price_staleness_seconds[10m])`). The absent branch has no `asset` label (aggregator tick wedged): go to `aggregator.md#stellarindex_aggregator_silent`. P2. `asset` is an aggregator pair base (`native`, `crypto:XLM`, `crypto:BTC`, `crypto:ETH`). `/v1/price` still answers 200 with an old `observed_at`.

**Check** (5 min).

```sh
# Which branch fired? No `asset` label => absent branch => aggregator.md#stellarindex_aggregator_silent
curl -s http://localhost:9090/api/v1/alerts |
  jq '.data.alerts[] | select(.labels.alertname=="stellarindex_api_price_stale") | {labels, value, activeAt}'

# Which asset and quote are stale? (aggregator metrics endpoint, 127.0.0.1:9465)
curl -s http://127.0.0.1:9465/metrics |
  awk '/^stellarindex_price_staleness_seconds/ && $2 > 120 {print}'

# Which sources quote this asset? (on-chain XLM rows are stored as
# `native`, CEX rows as `crypto:XLM`; the dual-form alias
# accepts both at the API.)
psql -d stellarindex -c "SELECT source, max(ts) AS most_recent
         FROM trades WHERE base_asset IN ('native', 'crypto:XLM')
                        OR quote_asset IN ('native', 'crypto:XLM')
         GROUP BY source ORDER BY most_recent DESC;"

# Is the aggregator binary running, ticking, and writing VWAPs?
ssh root@<host> "systemctl status stellarindex-aggregator --no-pager | head -10"
ssh root@<host> "curl -s http://127.0.0.1:9465/metrics | grep -E '^stellarindex_aggregator_(vwap_writes_total|ticks_total|empty_windows_total)'"
ssh root@<host> "journalctl -u stellarindex-aggregator -n 200 --output=cat | grep -i 'tick\|error' | tail -30"
```

**Likely causes.**

- Source stopped: `stellarindex_source_last_event_unix{source=<X>}` frozen. Events advancing while `stellarindex_source_last_insert_unix` (both on `127.0.0.1:9464`) is frozen is the stuck-cursor signature: `ledger-ingest.md#stellarindex_ingestion_cursor_stuck`, `ingestion-sink.md#stellarindex_ingestion_duplicate_flood`. Fix: `ingestion.md#stellarindex_ingestion_source_stopped`.
- CAGG refresh failing: `timescale.md#stellarindex_timescale_cagg_stale`.
- Pair not published for over 120 s: below the `min_usd_volume` gate (`/etc/stellarindex.toml`), empty windows (`stellarindex_aggregator_empty_windows_total`), anomaly freeze (`anomaly.md#stellarindex_anomaly_freeze_engaged`), fiat legs (`external-pollers.md`).
- Long-tail pair with no on-chain trades, or binary version skew (`binary-version-skew.md`).

**Fix.**

1. No `asset` label: `aggregator.md#stellarindex_aggregator_silent`.
2. One source dead: `ingestion.md#stellarindex_ingestion_source_stopped`. Pipeline problem: `timescale.md#stellarindex_timescale_cagg_stale`.
3. No on-chain activity: decide with product whether to de-list or keep the number with `stale=true`.
4. Verify the gauge is under 120 s and the alert clears.

**Escalate:** none (P2); chain halts surface as `stellar-node.md#stellarindex_stellar_core_ledger_age`; see also `sla-probe.md#stellarindex_sla_probe_freshness_breach`, `data-freshness.md#stellarindex_data_source_stale`, `data-freshness.md#stellarindex_served_value_drift`.

## stellarindex_api_price_stream_not_delivering

**Trigger.** `sum(rate(stellarindex_aggregator_stream_publish_total{outcome="ok"}[15m])) > 0 unless on() sum(rate(stellarindex_api_stream_subscribe_total{outcome="ok"}[15m])) > 0`, `for: 15m`, P3. The aggregator publishes but the API subscribes to nothing: `/v1/price/stream` sends keepalives and no `price_update`.

**Check.**

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=sum by (outcome) (increase(stellarindex_api_stream_subscribe_total[15m]))' | \
  jq -r '.data.result[] | "\(.metric.outcome): \(.value[1])"'
```

**Likely causes.**

- `future_observed_at` rising: API host clock lags the aggregator by over 5 min (`timedatectl` / chrony on both).
- `stale_observed_at` rising: events older than 24 h (replay or aggregator clock behind).
- `malformed` / `decode_error`: wire-format drift (version skew).
- Nothing rising: no messages received; go-redis retries a dead pubsub silently. Check `redis-cli PUBSUB NUMSUB stellarindex:closed-bucket:v1` and API stderr for reconnect lines.

**Fix.**

1. Clock skew: resync NTP on the lagging host.
2. No messages: confirm Redis is reachable, then restart the API for a fresh SUBSCRIBE.
3. Verify `outcome="ok"` rising for 15 min.

**Escalate:** none; if the aggregator is not publishing see `aggregator.md#stellarindex_aggregator_silent`.

## stellarindex_auth_reaper_stalled

**Trigger.** `time() - stellarindex_auth_reaper_last_sweep_unix{reaper}` exceeds `3 * stellarindex_auth_reaper_interval_seconds{reaper}` for 15m (also if the interval series exists with no last-sweep series). P3. A dead reaper freezes its rows gauge, so the `_table_growing` alerts fire only after the table has filled; this is the liveness signal. `reaper` is `login_code` ([lockout table](api.md#stellarindex_login_code_lockout_table_growing)), `magic_link` ([token table](#stellarindex_magic_link_token_table_growing)), `signup`, `session` or `webhook_delivery` (the last two, `internal/retentionreaper`, have no rows gauge; check `stellarindex_retention_reaper_errors_total{reaper}`).

**Check.**

```promql
stellarindex_auth_reaper_last_sweep_unix
stellarindex_auth_reaper_interval_seconds
```

```sh
journalctl -u stellarindex-api --since -6h | grep -iE "reaper|panic|recovered"
```

A `worker panicked` line names the reaper ([worker-panicked](infra.md#stellarindex_worker_panicked)); no line means a blocked goroutine, usually a long `DELETE FROM login_code_lockouts` / `magic_link_tokens` in `pg_stat_activity`. A reaper disabled in config publishes no series.

**Likely causes.** A panic (fix the bug), a lock held by a long transaction, or a wiring regression in `cmd/stellarindex-api/main.go`.

**Fix.**

1. Restart the dead goroutine: `systemctl restart stellarindex-api`. Reapers sweep on start; no customer impact beyond the restart.
2. Stuck Postgres call: `pg_terminate_backend` the blocker first, or the restart waits on the same lock.
3. A manual sweep of a large table is safe; reapers delete only settled/expired rows.

**Escalate:** none (P3); [metrics reference](../../reference/metrics/README.md#stellarindex_auth_reaper_last_sweep_unix).

## stellarindex_magic_link_token_table_growing

**Trigger.** [`stellarindex_magic_link_token_rows`](../../reference/metrics/README.md#stellarindex_magic_link_token_rows) `> 10000` for 30m, ticket. The sweep is alive but outpaced, or failing.

**Check.**

1. `rate(stellarindex_magic_link_token_rows_deleted_total[1h])` against the gauge growth: near zero means the sweep fails (API log `magiclinkreaper`); high means a mint flood.
2. Flood: `sum(rate(http_requests_total{route="/v1/auth/login"}[15m]))` and the per-IP throttle counters; block the source at the edge.
3. A dead sweep also fires [auth_reaper_stalled](#stellarindex_auth_reaper_stalled); fix that first.

## stellarindex_ch_schema_probe_absent

**Trigger.** `stellarindex_ch_schema_probe_present{probe="..."} == 0` for 30m on one API instance, P3. The explorer reader (`internal/storage/clickhouse/explorer_reader.go`) latches a "no such table/column" answer until restart, so the API stays on a slow fallback (or 503s). `0` = absent or empty; an unanswering lake moves `stellarindex_ch_schema_probe_unanswered_total` instead. A restart clears the series until the probe next answers, so the alert resolving on a deploy is not a fix: re-check step 1 after the first explorer read.

Each `probe` names the lake table `stellar.<probe>` (e.g. `tx_hash_index`, `contract_active_ledgers`, `accounts_stats`, `asset_holders_rollup`, `ops_by_source`, `account_activity`) except: `tx_hash_index_coverage` is a marker (apply `deploy/clickhouse/tx_hash_index_coverage.sql`, then backfill or insert the row); `contract_instance_changes_tx_key` is the `tx_hash` + `intra_ledger_seq` columns on `stellar.contract_instance_changes`; `ledger_entries_current_version` is the `version` column on `stellar.ledger_entries_current`. `ops_by_source_proof` is ch-index-proof's row in `stellar.entry_history_watermark`: account history serves without it, unproven. Run `stellarindex-ops ch-index-proof -ch-addr 127.0.0.1:9300 -write` under `run-heavy-job.sh` (about 70 s per million ledgers); a mismatch names the window to refill with `deploy/clickhouse/ops_by_source.sql` Step 2, then re-run from genesis.

**Check** (5 min).

```sh
# 1. Which probes read 0 on this API?
ssh r1 'curl -s http://localhost:3000/metrics | grep ^stellarindex_ch_schema_probe'

# 2. Does the object exist, and does it hold rows? (substitute the table)
clickhouse-client --port 9300 -q "EXISTS TABLE stellar.tx_hash_index"
clickhouse-client --port 9300 -q "SELECT count() FROM stellar.tx_hash_index"

# 3. For a column probe, is the column there?
clickhouse-client --port 9300 -q "SELECT name FROM system.columns
  WHERE database = 'stellar' AND table = 'ledger_entries_current' AND name = 'version'"
```

**Likely causes.**

- Object missing: DDL in `deploy/clickhouse/` not applied.
- Present and populated, gauge still `0`: verdict latched before the DDL landed.
- Present but empty: backfill not run or table truncated.

**Fix.**

1. Missing: apply the DDL from `deploy/clickhouse/` (file named after the table), then step 2.
2. Latched: `ssh r1 'sudo systemctl restart stellarindex-api'`; the gauge reads `1` after the first request that exercises that surface.
3. Empty: run the backfill documented beside the DDL; no restart needed.

An optional object a deployment deliberately omits is a standing ticket; silence with an expiry. **Escalate:** [clickhouse](clickhouse.md) if the lake itself is down; metrics in the [reference](../../reference/metrics/README.md).

## stellarindex_explorer_schema_absent_empty

**What.** An explorer read of `stellar.<table>` got "table/column does not exist" and served an empty result. The page shows no entry changes, trustline days or sponsorships, which reads as true.

**Fix.** Apply the table's DDL from `deploy/clickhouse/`, then run the backfill documented beside it (`account_entry_ledgers`/`asset_entry_ledgers`: entry-index; `sponsorship_events`: its backfill). The counter stops on the next read; the alert clears an hour later. **Escalate:** [clickhouse](clickhouse.md); metrics in the [reference](../../reference/metrics/README.md).

## stellarindex_explorer_exact_fallback_sustained

Ticket. `sum by (read) (rate(stellarindex_explorer_exact_fallback_total[30m])) > 0.05` for 1h. Explorer page reads (recent operations, account transactions/operations key arms, account and asset movements, contract events) read a bounded window in sort-key order and dedup it in Go; when duplicates fill the window, they re-run as the exact query over the whole history. Correct, but seconds per page.

Fix:

1. Find the table behind `read` (`sourced_*_keys`: `stellar.ops_by_source`; `*_movements`: `stellar.account_movements`; `recent_operations`: `stellar.operations`; `contract_events`: `stellar.contract_events`) and count its active parts per partition in `system.parts`; a pile of small parts means merges are behind.
2. Check `system.merges` for a stuck merge, and whether a backfill or replay is re-inserting rows into old partitions.
3. Once merges catch up the rate falls on its own; if one whale account drives it, raise that read's window factor in `explorer_window_dedup.go`.

## stellarindex_derived_index_watermark_missing

**What.** `stellar.entry_history_watermark` has no row for the index (or the table is missing). Reads gated on it (contract activity walk, contract stats, sponsorships, entry and signer history) serve their scan or lower-bound fallback.

**Fix.** Run the job that proves the index and stamps its row: `contract_active_ledgers` → `ch-contract-ledgers-backfill -genesis -full`; `contract_instance_changes` → `ch-instance-backfill`; `entry_index`/`signer_index` → entry-index.timer; `sponsorship_events` → `ch-entry-history`. The gauge updates on the next gated read. **Escalate:** [clickhouse](clickhouse.md); metrics in the [reference](../../reference/metrics/README.md).

## stellarindex_customer_webhook_delivery_failing

**Trigger.** `sum(rate(stellarindex_customer_webhook_delivery_attempts_total{outcome=~"server_error|network_error"}[5m])) > 0.05` for 15m, P3. Customers are not receiving callbacks (including SEV-1 pings); the API is unaffected. Retries back off 30s to 1h with a 15-attempt budget (about 4-8 h). The 15-attempt budget lands the last retry ~4–8 h after the event.

**Check.**

```sh
# Which webhook is failing? Group recent failures by webhook_id.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT webhook_id,
         COUNT(*)                              AS attempts,
         MAX(last_response_status)             AS worst_status,
         MAX(last_error)                       AS sample_error,
         BOOL_OR(delivered_at IS NOT NULL)     AS any_delivered_recently
    FROM webhook_deliveries
   WHERE created_at > now() - interval '\''30 min'\''
     AND (last_response_status >= 500 OR last_error LIKE '\''POST%'\'')
   GROUP BY webhook_id
   ORDER BY attempts DESC
   LIMIT 10;
"'

# Look up the customer + URL for the top offender:
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT w.id, w.account_id, a.name, a.billing_email, w.url, w.enabled
    FROM customer_webhooks w JOIN accounts a ON a.id = w.account_id
   WHERE w.id = '\''<webhook_id from above>'\'';
"'

ssh r1 'curl -s http://localhost:3000/metrics | grep stellarindex_customer_webhook_delivery_attempts_total | head -10'

ssh r1 'curl -s http://localhost:3000/metrics | grep stellarindex_customer_webhook_delivery_duration_seconds | head -20'
```

**Likely causes.** One webhook failing while others deliver is a single-customer outage; zero `delivered` anywhere is the worker (`journalctl -u stellarindex-api -g "customer-webhook"`) or R1 egress (try `curl -X POST https://example.com/hook -d '{}'`); an unset `STELLARINDEX_WEBHOOK_SEAL_KEY` (vault `vault_webhook_seal_key`) makes sealed webhooks count `outcome="lookup_error"`, and a changed key is terminal (`no_secret`).

**Fix.**

1. Capture webhook ID, account ID, `billing_email`, URL and `last_error`.
2. Single customer: contact them via `billing_email`; updating the URL via `PATCH /v1/dashboard/webhooks/{id}` re-arms delivery.
3. Worker failing:

   ```sh
   # Inspect logs for the underlying error (timeout / TLS / DNS).
   ssh r1 'journalctl -u stellarindex-api --since "30 min ago" \
     | grep -E "customer-webhook|delivery (delivered|failed)"'
   # Restart only as a last resort — the worker is in-process
   # with the API; restart paths cycle every cached state.
   ssh r1 'systemctl restart stellarindex-api'
   ```

**Escalate:** none (P3). Code: `internal/customerwebhook/worker.go`; upstream trigger [anomaly-freeze-engaged](anomaly.md#stellarindex_anomaly_freeze_engaged). This covers outbound deliveries only.

## stellarindex_customer_webhook_delivery_exhausted

**Trigger.** `sum(rate(stellarindex_customer_webhook_delivery_attempts_total{outcome="exhausted"}[1h])) > 0`, `for: 0m`, P3. A delivery hit the retry budget and is terminally failed; the customer never got the event.

**Check.** Same queries as [delivery_failing](#stellarindex_customer_webhook_delivery_failing).

**Fix.** The row stays in `webhook_deliveries` for forensics and there is no automatic retry. Cleanest: ask the customer to `PATCH` the webhook URL or trigger a fresh event; re-enqueue manually only by operator decision. **Escalate:** none.

## stellarindex_customer_webhook_mark_errors

**Trigger.** `increase(stellarindex_customer_webhook_delivery_attempts_total{outcome="mark_error"}[30m]) > 0` for 15m, P3. Read this first when it fires with the other two. The POST decided an outcome but the store write failed; only that write advances `attempt_count`, so the 5-minute claim lease repeats and the SAME payload is re-POSTed indefinitely (customer sees duplicates). Sites: `classifyResponse`, `handleFailure`, `markTerminal` in `internal/customerwebhook/worker.go`; each logs a WARN with `delivery_id`.

**Check.**

```sh
# Which deliveries are wedged? A high attempt-lease churn with a
# STATIC attempt_count is the signature.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT id, webhook_id, event_type, attempt_count, next_attempt_at, created_at
  FROM webhook_deliveries
  WHERE next_attempt_at IS NOT NULL AND next_attempt_at < now() + interval '"'"'10 min'"'"'
  ORDER BY next_attempt_at DESC LIMIT 20"'

# The worker's own account of it.
ssh r1 'journalctl -u stellarindex-api --since -2h | grep -i "customer-webhook.*Mark"'
```

**Fix.** Restore the database; the next lease writes the outcome and the loop ends. A contended row retries at lease expiry with a fresh budget. **Escalate:** [postgres](postgres.md#stellarindex_timescale_primary_down) if the DB is down.

## stellarindex_dex_tvl_refresh_failing

**Trigger.** `sum(rate(stellarindex_dex_tvl_refresh_total{outcome="error"}[15m])) > 0 and sum(rate(stellarindex_dex_tvl_refresh_total{outcome="ok"}[15m])) == 0` for 30m, ticket. It requires zero `ok`, so it cannot see a single-protocol failure (that is [total_divergent](#stellarindex_dex_tvl_total_divergent)). `/v1/protocols` and per-protocol TVL keep serving a carried-forward snapshot whose `as_of` stops advancing; a protocol that has never read successfully is absent and named in `tvl_total.excluded` (sets `lower_bound`).

**Check.**

```sh
# Which protocol is failing? The refresh error is per-protocol:
journalctl -u stellarindex-api --since -30min | grep "dex tvl cache refresh" | tail -3
# Lake-side reads (soroswap/phoenix/comet reserves) -> ClickHouse:
clickhouse-client --port 9300 -q "SELECT 1"
# Served-tier reads (aquarius reserve snapshots, prices_1m USD legs) -> Postgres:
sudo -u postgres psql stellarindex -c "SELECT max(bucket) FROM prices_1m"
```

Backends healthy but still erroring: chart `stellarindex_dex_tvl_refresh_duration_seconds`; an `error` histogram pinned at 180 s means reads time out (a scan lost its `max_threads`/`max_memory_usage` pin).

**Likely causes.**

- ClickHouse unreachable (lake reserve reads).
- Postgres served tier unreachable (`prices_1m`).
- Lake scan timing out at the 3-minute limit.

**Fix.**

1. Fix the failing backend; the next 10-min tick repopulates every protocol. No restart.
2. One protocol erroring (alert silent): its reader broke (e.g. lake schema change); file it.
3. Restart `stellarindex-api` only if wedged: it clears the snapshot and the TVL field is omitted until the first good refresh.

A ClickHouse restart or merge can fail a refresh or two (absorbed by `for: 30m`). **Escalate:** none; related [sdex_orderbook_maintain_failing](#stellarindex_sdex_orderbook_maintain_failing), `internal/api/v1/dex_tvl_cache.go`.

## stellarindex_dex_tvl_total_divergent

**Trigger.** `sum(rate(stellarindex_dex_tvl_reconcile_total{outcome="divergent"}[30m])) > 0 and sum(rate(stellarindex_dex_tvl_reconcile_total{outcome="ok"}[30m])) == 0` for 1h, ticket. The headline `tvl_total` understates: it excludes a protocol whose figure is still published beside it (no 5xx, every refusal named in `excluded[]`). `dexTVLAdmit` (`internal/api/v1/dex_tvl_total.go`) refuses a carried-forward, mismatched-`as_of`, unbalanced-pool or unparsable figure. Standing `excluded[]` scope entries (CAP-38 pools, SDEX book, lending, vault AUM) are not refusals.

**Check.**

```sh
# Which protocol was refused, and under which rule?
curl -s localhost:3000/v1/protocols \
  | jq '.data.tvl_total | {protocols, lower_bound, as_of, excluded}'
# Compare against which rows DO carry a per-protocol figure: the
# difference is the refused set.
curl -s localhost:3000/v1/protocols \
  | jq -r '.data.protocols[] | select(.tvl) | .name'
# A carried-forward figure means that protocol's reserve read failed:
journalctl -u stellarindex-api --since -60min | grep "dex tvl" | tail -5
# The refused protocol's figure and pools are still served, labelled,
# on the drill-down — carried_forward: true (flags.stale too):
curl -s localhost:3000/v1/protocols/<name>/tvl \
  | jq '{carried: .data.carried_forward, tvl: .data.tvl, pools: (.data.pools | length)}'
```

**Likely causes.**

- Failed reserve read (tracks `stellarindex_dex_tvl_refresh_total{outcome="error"}`): almost always.
- Code defects in `internal/api/v1/dex_tvl_cache.go` / `dex_tvl_total.go`: pool counts not balancing (`pools_priced + unpriced_pools != pools_total`) or `tvl_usd` not parsing.

**Fix.**

1. Fix the failing backend; the next 10-min refresh readmits the protocol. No restart. A backend outage is primary ([refresh_failing](#stellarindex_dex_tvl_refresh_failing)).
2. Verify `stellarindex_dex_tvl_reconcile_total{outcome="ok"}` increments within 10 min.
3. Do NOT loosen admission: absorbing a carried-forward figure is a wrong number under a fresh `as_of`.

After an API restart no reconciliation has run and the alert cannot fire. **Escalate:** none; methodology `docs/methodology/dex-tvl.md`.

## stellarindex_directory_sync_unflagged

**Trigger.** `max by (instance, source) (stellarindex_directory_sync_rows_changed{kind="unflagged"}) > 0`, P3. `stellarindex-ops directory-sync` mirrors the stellar-expert public directory into `account_directory`; a committed run that removed a scam-class tag or pruned a flagged row means `internal/pricingguard.ScamGate` stops withholding that issuer's price. The gauge holds the last committed run and clears on the next daily run with none. One run may clear at most `max(10, ceil(5 % of previously flagged))` tags, else `ErrDirectoryChurnExceeded` unless `-accept-churn`.

**Check.**

1. `journalctl -u directory-sync | grep 'Synced:'` (upserted, pruned, newly-flagged, un-flagged).
2. Compare the upstream repository history for the run's day; a tag removed in a reviewed commit is a genuine correction.
3. Pinned? `grep EXTRA_FLAGS /etc/default/directory-sync`; without `-sha256` the run took whatever the branch held.

**Fix.**

1. Genuine correction: no action.
2. Not genuine: set `EXTRA_FLAGS=-url https://github.com/stellar-expert/public-directory/archive/<commit>.tar.gz -sha256 <hex digest of that tarball>` in `/etc/default/directory-sync`, then `systemctl start directory-sync`.
3. Do NOT use `-accept-churn` unless the mass change is confirmed genuine, and do NOT silence this alert: it is the only signal that a flagged issuer is priced again.

**Escalate:** none; related [scam_gate_fail_open](#stellarindex_scam_gate_fail_open), [systemd-unit-failed](infra.md#stellarindex_systemd_unit_failed).

## stellarindex_failed_auth_rate_high

**Trigger.** `sum(rate(stellarindex_failed_auth_total{outcome="rejected"}[5m])) > 1` for 15m, P3. Credential guessing may be underway; nothing got through. The throttle caps failures per IP and key prefix at `api.failed_auth_rate_limit_per_min` (default 20/min), so a sustained 1/s means more than three sources at cap or many under it.

**Check.**

```promql
sum by (outcome) (rate(stellarindex_failed_auth_total[5m]))
```

```sh
journalctl -u stellarindex-api --since '30 min ago' --no-pager -o cat \
  | jq -r 'select(.status == 401 or .status == 403) | .remote_ip' \
  | sort | uniq -c | sort -rn | head -20
```

**Fix.**

1. Known customer: tell them about the stale key.
2. Distributed guessing: block the source ranges at the edge (keys carry a 224-bit secret; the cost is validator load).
3. Verify the rejected rate is under 1/s. Never set `api.failed_auth_rate_limit_per_min` to zero.

**Escalate:** none; the throttle shares Redis with [ratelimit_fail_closed](#stellarindex_ratelimit_fail_closed).

## stellarindex_login_code_lockout_table_growing

**Trigger.** Two arms, `for: 30m`, P3: rows `stellarindex_login_code_lockout_rows > 10000` (healthy is low tens), or fail-open `increase(stellarindex_login_code_lockout_errors_total{op="status_check"}[1h]) > 0` (the pre-match lockout read errored and the handler failed open; the 6-digit code brute-force bound is silently off). Table `login_code_lockouts` (migration 0122) is filled by unauthenticated `POST /v1/auth/verify-code`; `internal/logincodereaper` (hourly, 48 h retention) bounds it.

**Check.**

```promql
stellarindex_login_code_lockout_rows
sum by (op) (increase(stellarindex_login_code_lockout_errors_total[1h]))
```

```sh
# Fail-open arm: migration 0122 applied on every node?
ssh r1 'sudo -u postgres psql stellarindex -c "\d login_code_lockouts"'
ssh r1 "journalctl -u stellarindex-api --since '-2h' | grep 'login code lockout status unavailable'"

# Rows arm: is the sweep running? Expect a start line at boot plus
# `deleted settled rows` whenever it removes anything.
ssh r1 "journalctl -u stellarindex-api --since '-6h' | grep 'login-code-lockout reaper'"

# Fill or real traffic? A fill is many addresses with tiny failed_count.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT failed_count, count(*) AS addresses, min(created_at), max(created_at)
    FROM login_code_lockouts GROUP BY 1 ORDER BY 2 DESC LIMIT 10;"'
```

Thousands at `failed_count = 1` is a fill; a few at `>= 10` is a grinder (control working).

**Fix.**

1. Fail-open: apply migration 0122 or fix the failing read. Until then only the per-token `maxCodeAttempts = 5` cap applies.
2. Sweep broken: fix Postgres; a manual pass is safe and keeps BOTH predicates (the second protects live locks):

   ```sql
   DELETE FROM login_code_lockouts
    WHERE updated_at < now() - interval '48 hours'
      AND (locked_until IS NULL OR locked_until <= now());
   ```
3. Active fill: bounded by `api.anon_rate_limit_per_min` (default 60); confirm it is non-zero, block a single-origin IP at the edge.
4. Do NOT gate the INSERT on live tokens (enumeration oracle), shorten retention below 24 h (`dashboardauth.durableCodeFailureWindow`), or make the fail-open fail-closed.

**Escalate:** none; related [ratelimit_fail_open](#stellarindex_ratelimit_fail_open), [admin_audit_write_failing](api.md#stellarindex_admin_audit_write_failing).

## stellarindex_monthly_quota_fail_closed

**Trigger.** `sum(rate(stellarindex_monthly_quota_fail_closed_total[5m])) > 0` for 2m, P1. After counter reads fail longer than `DefaultMonthlyQuotaDwellTime` (30s, `api.monthly_quota_dwell`) the gate fails closed: every metered request gets `429` (`errors/monthly-quota-unavailable`). Paying customers are denied. Clears after the same dwell of clean reads.

**Check / Likely causes / Fix.** As [monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open), with Redis recovery urgent. It usually moves with [ratelimit_fail_closed](#stellarindex_ratelimit_fail_closed) (shared Redis); handle that outage first. Do NOT shorten the dwell time. **Escalate:** page; SEV per [sev-playbook](../sev-playbook.md).

## stellarindex_monthly_quota_fail_open

**Trigger.** `sum(rate(stellarindex_monthly_quota_fail_open_total[5m])) > 0` for 10m, P3. `internal/api/v1/middleware/monthly_quota.go` fails open when the month-to-date counter read errors, so every metered key is uncapped; metering still records and overage cannot be reclaimed. The rate is requests/sec bypassing the cap (only keys with `monthly_quota > 0`). Write-side twin: [usage_write_failing](#stellarindex_usage_write_failing).

**Check.**

1. Redis up? `systemctl status redis` on r1, then `redis-cli -a "$REDIS_PASSWORD" ping`; the `/readyz` redis check should be red if not.
2. `stellarindex_ratelimit_fail_open` also firing: a plain Redis incident; handle that first.
3. Alone: usage-counter read path. `journalctl -u stellarindex-api | grep -i 'monthly-quota'` shows `NOAUTH` / `WRONGPASS` (AUTH drift) or `context deadline exceeded` (pool exhaustion).

**Fix.**

1. Redis down: recovery path; the middleware self-heals on the first good read, alert clears about 10 min later.
2. AUTH drift: re-sync `STELLARINDEX_REDIS_PASSWORD` in `/etc/default/stellarindex` to Redis's `requirepass` (ansible `redis_password`), then `systemctl restart stellarindex-api`.
3. Eviction: counters must not be in an evictable class (Redis config fix).
4. After recovery reconcile overage from the usage rollup, not this counter.
5. Do NOT shorten the dwell time or silence while Redis is down.

**Escalate:** none; related [admin_audit_write_failing](api.md#stellarindex_admin_audit_write_failing), [metrics-registry-absent](meta.md#stellarindex_metrics_registry_absent).

## stellarindex_passkey_clone_warning

**Trigger.** `sum(increase(stellarindex_passkey_login_refusals_total{reason="clone_warning"}[1h])) > 0` for 5m, P3. `POST /v1/auth/passkey/finish-login` refused an assertion whose signature verifies but whose counter is at or below the stored one (two copies of a hardware/device-bound key), and wrote a `passkey.clone_warning` row to `audit_log`. No session was issued. Counter-0 authenticators (iCloud, synced passkeys) are exempt.

**Check.**

```sql
-- Which account and credential, from where.
SELECT ts, account_id, target_id AS credential_row_id, ip, user_agent, metadata
FROM audit_log
WHERE action = 'passkey.clone_warning'
ORDER BY ts DESC
LIMIT 20;
```

`metadata` has `credential_owner_user_id`, `credential_name`, `stored_sign_count`, `presented_sign_count`. Account history (`passkey.register`, `passkey.delete`, `passkey.login_replay`):

```sql
SELECT ts, action, actor_kind, actor_user_id, target_id, ip, metadata
FROM audit_log
WHERE account_id = '<account_id>' AND action LIKE 'passkey.%'
ORDER BY ts DESC;
```

**Fix.**

1. Contact the owner (`credential_owner_user_id`): remove the named passkey in dashboard security settings and register a new one.
2. Owner unreachable and IPs hostile: delete the `webauthn_credentials.id = target_id` row and revoke the user's sessions; record who and why.
3. Verify no new rows for that credential; the alert clears an hour after the last refusal.
4. Do NOT treat it as an outage or reset the stored sign count.

**Escalate:** none. If the audit row failed to land, [admin_audit_write_failing](api.md#stellarindex_admin_audit_write_failing) fires with `surface="passkey_clone_warning"`. See [ADR-0049](../../adr/0049-anonymous-access-and-passkey-auth.md).

## stellarindex_ratelimit_fail_closed

**Trigger.** `sum(rate(stellarindex_ratelimit_fail_closed_total[5m])) > 0` for 2m, page. Past `ratelimit.DefaultDwellTime` (30s) of continuous Redis errors on a bucket, the limiter returns 503 (`errors/throttle-unavailable`, `Retry-After: 30`): a hard sustained Redis outage with a red Redis readiness check. Clears after the same dwell of clean Redis answers; anonymous and authenticated buckets have separate clocks.

**Check / Likely causes / Fix.** As [ratelimit_fail_open](#stellarindex_ratelimit_fail_open); Redis recovery is urgent. Do NOT disable fail-closed (a negative `WithDwellTime`): degrading Redis would then give an attacker unlimited volume. **Escalate:** page; [redis-master-down](cache.md#stellarindex_redis_master_down).

## stellarindex_ratelimit_fail_open

**Trigger.** `sum(increase(stellarindex_ratelimit_fail_open_total[15m])) > 100`, `for: 0m`, P3. The Redis fixed-window limiter (`internal/ratelimit`; `internal/api/v1/middleware/ratelimit.go`) fails open on Redis errors, then closed after `ratelimit.DefaultDwellTime` (30s; [fail_closed](#stellarindex_ratelimit_fail_closed)), so this alert sees only repeated short episodes (hence `increase`). Per-key limits are not enforced meanwhile; usage still meters.

**Check.**

1. Redis up? `systemctl status redis` on r1, then `redis-cli -a "$REDIS_PASSWORD" ping`; `/readyz` redis checker.
2. Alone (Redis healthy): `journalctl -u stellarindex-api | grep -i 'ratelimit\|redis'` for `NOAUTH` / `WRONGPASS`, `context deadline exceeded`.
3. Exposure: `sum(rate(stellarindex_ratelimit_fail_open_total[5m]))` equal to `sum(rate(http_requests_total[5m]))` means nothing is limited.

**Fix.**

1. Redis down: recovery path; the alert clears about 10 min after the last bypass.
2. AUTH drift: re-sync `STELLARINDEX_REDIS_PASSWORD` in `/etc/default/stellarindex` to Redis's `requirepass` (ansible `redis_password`), then `systemctl restart stellarindex-api`.
3. Abuse while open: block the source at the edge (see the shed-load step of [error_rate_critical](#stellarindex_api_error_rate_critical)).
4. Do NOT remove the dwell window or silence while Redis is down.

**Escalate:** none; related [metrics-registry-absent](meta.md#stellarindex_metrics_registry_absent), [api_down](#stellarindex_api_down).

## stellarindex_scam_gate_fail_open

**Trigger.** `sum by (surface) (rate(stellarindex_scam_gate_lookup_failures_total[5m])) > 0` for 5m, P3. `internal/pricingguard.ScamGate` fails open on a lookup error (failing closed would blank every price on a DB blip), so directory-flagged issuers' prices are served at 200. Logs `scam pricing gate: directory lookup failed — serving unguarded`. `surface` is the serving path (`price_read`, `vwap`, `chart`, ...; `price_alert` is the aggregator): one surface points at that path, all at the database.

**Check.**

1. Postgres reachable: API `/readyz`.
2. `journalctl -u stellarindex-api | grep 'scam pricing gate'` (`-u stellarindex-aggregator` for `price_alert`); the WARN `err` names the cause.
3. `psql "$DATABASE_URL" -c 'SELECT count(*) FROM account_directory'`; a hang is a lock from a long `directory-sync` transaction or a migration.

**Fix.**

1. Database: recovery path; the gate self-heals on the next good lookup (verdicts cached 60 s).
2. `directory-sync` lock: let it finish or cancel it (safe to re-run).
3. Pool exhaustion: relieve the load.
4. Do NOT fail closed.

**Escalate:** none; related [directory_sync_unflagged](#stellarindex_directory_sync_unflagged), `stellarindex_price_serve_scam_withheld_total` (dropping to zero while this fires is the same outage), [metrics reference](../../reference/metrics/README.md).

## stellarindex_sdex_orderbook_maintain_failing

**Trigger.** `sum(rate(stellarindex_sdex_orderbook_maintain_total{outcome=~"load_error|advance_error|verify_error"}[15m])) > 0` for 30m, ticket. The four sdex_orderbook alerts share this diagnosis; check which outcome fired. All failures are ClickHouse lake reads. Impact: `load_error` means `/v1/sdex/orderbook` 503s (warming); `advance_error` means stale depth (`as_of_ledger` stops); `verify_error` means the version-tie quarantine stops draining (`ask_offers_withheld` / `bid_offers_withheld`).

**Check.**

```sh
# Which mode? load_error / advance_error / verify_error:
curl -s localhost:9464/metrics | grep sdex_orderbook_maintain_total

# Quarantine size and crossed pairs:
curl -s localhost:9464/metrics | grep -E 'sdex_orderbook_(pending_offers|crossed_pairs) '

# Which pairs are crossed (logged on every change of the count):
journalctl -u stellarindex-api --since -2h | grep "crossed-pair count changed" | tail -3

# Lake health — both paths read ClickHouse:
clickhouse-client --port 9300 -q "SELECT 1"

# If load_error: how long are attempts running before dying?
curl -s localhost:9464/metrics | grep 'sdex_orderbook_maintain_duration_seconds.*load_error' | tail -3
```

Also `journalctl -u stellarindex-api | grep "sdex order book"`.

**Fix.**

1. `advance_error` with ClickHouse healthy: transient; the 60s tick retries idempotently.
2. `load_error` repeating: look for a heavy job saturating ClickHouse; it self-heals once the lake frees. Consistently hitting the 30-min cap on a healthy lake is a code/schema issue: file it, do not raise the cap.
3. `verify_error`: treat as a ClickHouse read failure (`sdex order book verify` journal line); the book stays honest but thinner.
4. Restart only if the maintainer goroutine is wedged; it re-quarantines every `intra_ledger_seq == 0` offer, so expect `pending_offers` and `*_offers_withheld` elevated for hours.

A deploy during a merge can fail the first load (absorbed by `for: 30m`). **Escalate:** none; related [dex_tvl_refresh_failing](#stellarindex_dex_tvl_refresh_failing), `internal/api/v1/sdex_orderbook.go`.

## stellarindex_sdex_orderbook_advance_held

**Trigger.** `sum(rate(stellarindex_sdex_orderbook_maintain_total{outcome="advance_held"}[15m])) > 0 unless sum(rate(stellarindex_sdex_orderbook_maintain_total{outcome="advance_ok"}[15m])) > 0` for 30m with no error outcome, ticket. The cursor is held at a fixed ledger by an unhealed `ledger_entry_changes` hole or an ingest halt (depth goes stale, no error).

**Check.** The API journal line "advance held below a lake hole" (cursor / contiguous_tip / lake_max); `ch-live-catchup`, `stellarindex_ingestion_cursor_stuck`, `stellarindex_ingestion_ledger_stalled`. Commands: [maintain_failing](#stellarindex_sdex_orderbook_maintain_failing).

**Fix.** Self-heals when the hole is healed or ingest resumes. **Escalate:** the ingest alerts above if a lake-wide halt.

## stellarindex_sdex_orderbook_reload_failing

**Trigger.** `sum by (instance) (increase(stellarindex_sdex_orderbook_maintain_total{outcome="load_error"}[3h])) > 0 unless sum by (instance) (increase(stellarindex_sdex_orderbook_maintain_total{outcome="load_ok"}[3h])) > 0` for 2h, ticket. The daily self-heal re-load keeps failing its hourly retry. The old book keeps serving (200), but changes below the cursor stay wrong until a re-load lands.

**Check / Fix.** As the `load_error` case in [maintain_failing](#stellarindex_sdex_orderbook_maintain_failing) (ClickHouse reachability, heavy-job saturation, 30-min cap). Clears on the first `load_ok`. **Escalate:** none.

## stellarindex_sdex_orderbook_crossed_book

**Trigger.** `max(stellarindex_sdex_orderbook_crossed_pairs) > 0` for 30m, ticket. A served pair has best bid > best ask (touching is not counted): a phantom offer whose on-chain removal the lake never ingested; verification cannot disprove it and the daily re-load reloads it.

**Check.** `journalctl -u stellarindex-api --since -2h | grep "crossed-pair count changed" | tail -3`, then `/v1/sdex/orderbook?selling=A&buying=B`.

**Fix.** Only healing the lake: chase the missing `ledger_entry_changes` removal with the completeness tooling. **Escalate:** none.

## stellarindex_tls_cert_expiring_soon

**Trigger.** `for: 1h`, P2, three arms. An expired cert fails every request; 14 days leaves time to renew by hand.

```promql
(
  stellarindex_tls_cert_not_after_unix - time() < 14 * 24 * 3600
  and stellarindex_tls_cert_not_after_unix > 0
)
or absent_over_time(stellarindex_tls_cert_not_after_unix[1h]) == 1
or (
  sum by (host) (increase(stellarindex_tls_cert_probe_total{outcome!="ok"}[13h])) > 0
  unless on (host)
  sum by (host) (increase(stellarindex_tls_cert_probe_total{outcome="ok"}[13h])) > 0
)
```

A failing probe keeps the last gauge value, so the absent and probe-outcome arms are the liveness signals. The probe runs in the API every 6 h (`internal/api/v1/tls_probe.go::RunTLSCertProbe`).

**Check.**

```sh
ssh root@<host>

# 1. Confirm the gauge value vs. NOW.
curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix

# 2. Read the actual on-disk cert Caddy is serving.
openssl x509 -in /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/api.stellarindex.io/api.stellarindex.io.crt -noout -enddate

# 3. Check Caddy's renewal log for the most recent attempt.
journalctl -u caddy --since "30d ago" --no-pager | grep -iE "renew|certificate"
```

**Likely causes.** ACME rate limit (`429` / `tooManyCertificatesPerName` in Caddy's journal); HTTP-01 failing (port 80 blocked, Cloudflare proxying); full root partition under `/var/lib/caddy`; Caddy stopped.

**Fix.** The live config is `/etc/caddy/Caddyfile`, rendered from `configs/ansible/roles/archival-node/templates/Caddyfile.j2`.

1. Trigger renewal without touching the cert, or reload (renewals attempt at startup), and watch:

   ```sh
   # Trigger renewal without touching the cert (admin endpoint):
   curl -X POST 'http://localhost:2019/load' --data-binary @/etc/caddy/Caddyfile -H 'Content-Type: text/caddyfile'

   # Or reload/restart (renewals attempt at startup):
   caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy

   # Watch the renewal attempt:
   journalctl -u caddy -f
   ```
2. Cannot renew (rate limited): use an alternate ACME directory via `acme_ca` in the site's `tls` block (not `acme_ca_root`), or certbot standalone. Codify the change in `Caddyfile.j2`; provisioning sequence in `docs/operations/r1-deployment-state.md`.

   ```caddyfile
   tls {
       acme_ca https://acme.zerossl.com/v2/DV90
   }
   ```
3. Verify (force a probe by restarting the API):

   ```sh
   systemctl restart stellarindex-api
   sleep 30

   curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix
   # Should show a NotAfter ~90 days in the future for Let's Encrypt.
   ```

The alert clears after `for: 1h`. **Escalate:** page the platform owner if under 3 days remain. Metric: [reference](../../reference/metrics/README.md#stellarindex_tls_cert_not_after_unix).

## stellarindex_usage_rollup_failing

**Trigger.** `sum(rate(stellarindex_usage_rollup_sweeps_total{outcome=~"scan_error|sink_error"}[15m])) > 0` for 30m, ticket (5-min sweeps, so 6+ failures; `outcome="ok"` flat separates failure from a quiet counter). Per-endpoint usage freezes; no pricing impact. Redis counters keep 35 days; after recovery the worker re-folds skipped days (7 per sweep).

**Check.**

```sh
# Which half is failing?
curl -s localhost:3000/metrics | grep stellarindex_usage_rollup_sweeps_total
# scan_error -> Redis. Check reachability + the counter keys:
redis-cli -n 0 --scan --pattern 'usage:ep:*' | head
# sink_error -> Postgres. Check the table exists (migration 0071):
sudo -u postgres psql stellarindex -c '\d usage_daily'
# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-api --since -30min | grep -i "usage rollup"
```

**Fix.**

1. Missing table: run the migrator (deploy.yml auto-applies; manual: `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up`).
2. Redis/Postgres down: follow the infra runbook; the worker retries forever and replays are safe. Check the days landed:

   ```sh
   sudo -u postgres psql stellarindex -c "SELECT day, count(*) FROM usage_daily WHERE day >= now() - interval '7 days' GROUP BY day ORDER BY day"
   ```
3. Fold a range immediately (or when no API runs the rollup); only works inside the 35-day window:

   ```sh
   # Size the recovery first (dry run, no writes):
   stellarindex-ops usage-rollup-backfill -config /etc/stellarindex.toml \
     -from 2026-07-19 -to 2026-07-21
   # Then apply it:
   stellarindex-ops usage-rollup-backfill -config /etc/stellarindex.toml \
     -from 2026-07-19 -to 2026-07-21 -write
   ```

**Escalate:** none. Metrics: [`stellarindex_usage_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_usage_rollup_sweeps_total), [`stellarindex_usage_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_usage_rollup_sweep_duration_seconds); worker `internal/usage/rollup.go`; [alerts-catalog.md](../alerts-catalog.md).

## stellarindex_usage_write_failing

**Trigger.** `sum(rate(stellarindex_usage_units_dropped_total{counter="billable"}[5m])) > 0` for 10m, P3. `UsageTracker` (`internal/api/v1/middleware/usage.go`) failed to write per-subject Redis usage counters: billable units go unmetered, and `MonthlyQuota` sees a low real value so nothing else signals the ceiling is uncapped. Write-side twin of [monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open) (read failure); confirm which fires.

**Check.**

1. Redis up: `redis-cli -a "$REDIS_PASSWORD" ping`, `/readyz`.
2. Alone: `journalctl -u stellarindex-api | grep -i 'usage: increment failed\|usage: detail increment failed'` (Debug level; raise the log level temporarily).
3. Exposure: `sum(rate(stellarindex_usage_units_dropped_total{counter="billable"}[5m]))` is request-units/sec unmetered.

**Fix.**

1. Redis down: recovery path; the tracker self-heals on the first good write.
2. ACL drift: confirm the API's Redis credential can write the `usage:` prefix.
3. After recovery, reconcile from the durable `usage_daily` rollup (a lower bound), not this alert's rate.
4. Do NOT silence it: it is a sustained silent revenue leak.

**Escalate:** none; related [ratelimit_fail_open](#stellarindex_ratelimit_fail_open).

## Related

- SLO burn alerts (`stellarindex_slo_*`) link to the `stellarindex_api_error_rate_critical` and `stellarindex_api_latency_p95_high` sections.
