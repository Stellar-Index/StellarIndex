---
title: Runbook — sla-probe
last_verified: 2026-08-28
status: living
severity: P2
---

# Runbook — SLA probe alerts

Six alerts, group `stellarindex.sla_probe`, rule file `configs/prometheus/rules.r1/sla-probe.yml` (the one r1 loads; multi-host twin `deploy/monitoring/rules/sla-probe.yml`, same expr/for/labels). Five are `severity: page` (P2); `stellarindex_sla_probe_unit_failed_alert` is `severity: ticket` (P3). All `component: api`. All `for: 30m` except `_stale` (`for: 5m`).

The probe (`cmd/stellarindex-sla-probe`) runs from the 15-minute timer `stellarindex-sla-probe.timer` through the wrapper `configs/healthchecks/sla-probe.sh`. It writes metrics to `/var/lib/node_exporter/textfile_collector/sla_probe.prom` (scraped via node_exporter's textfile collector) and POSTs the JSON report to Healthchecks.io. The JSON NEVER reaches journald, so `journalctl | jq` finds nothing. Read the last verdict from the textfile, the Healthchecks.io check's last ping body, or a one-off run.

Shared commands (on r1, `ssh root@136.243.90.96`; Prometheus listens on localhost):

```sh
# Last run's verdict
cat /var/lib/node_exporter/textfile_collector/sla_probe.prom

# One-off run; prints failed_reasons
export STELLARINDEX_PROBE_API_KEY=<key>   # or pass -api-key
/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 \
  -pair native,fiat:USD -duration 30s -concurrency 1 -report-format json | jq .failed_reasons
```

Probe gotchas that fake a breach:

- Keep `-concurrency 1`. At 2 the probe self-saturates the API (F-1305: ~2.5k req/s, p95 to 3.8 s) and measures its own load.
- Authenticate. A keyless run hits the anonymous 60/min limit and 429-fake-fails (F-1311, the 2026-08-24 GO-stack retirement shape). That reads as an AVAILABILITY fail, not a latency signal.
- A deploy restart inside a run or a cold cache after a deploy can trip one run; the 30 m `for` absorbs it.
- Probe host pinned at 100% CPU during another job: confirm on the host's own metrics.

Published targets (`/sla`): p95 <= 200 ms, p99 <= 500 ms, availability >= 99.9 % (2xx share), freshness 30 s (150 s for `/v1/price`).

## At a glance

- [`stellarindex_sla_probe_p95_breach`](#stellarindex_sla_probe_p95_breach) - p95 > 200 ms, page, MTTR 15-60 min
- [`stellarindex_sla_probe_p99_breach`](#stellarindex_sla_probe_p99_breach) - p99 > 500 ms, page, MTTR 15-60 min
- [`stellarindex_sla_probe_availability_breach`](#stellarindex_sla_probe_availability_breach) - 2xx share < 99.9 %, page, MTTR 15-60 min
- [`stellarindex_sla_probe_freshness_breach`](#stellarindex_sla_probe_freshness_breach) - observed_at over bound, page, MTTR 30-90 min
- [`stellarindex_sla_probe_unit_failed_alert`](#stellarindex_sla_probe_unit_failed_alert) - umbrella verdict, ticket
- [`stellarindex_sla_probe_stale`](#stellarindex_sla_probe_stale) - no passing run in 90 min, page, MTTR 15 min

## stellarindex_sla_probe_p95_breach

Page. Direct SLA breach: detail-page consumers see degraded responsiveness on the named endpoint.

Symptoms:

- `stellarindex_sla_probe_latency_ms{endpoint=...,quantile="0.95"} > 200` for >= 30 min (2 timer firings).
- The probe's latest report carries `failed_reasons: ["<endpoint>: p95=<N>ms > target 200.0ms"]`.
- `stellarindex_api_latency_p95_high` may also fire; complementary signals (probe = synthetic, histogram = real traffic).

Quick diagnosis (<= 5 min):

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

Root causes: same backend as `api.md#stellarindex_api_latency_p95_high` (see its "Typical root causes"), plus:

1. Probe-host network path issue. Probe slow but histogram fine: the problem is between the probe runner and the API edge. Confirm by running from a different network.
2. Endpoint-specific slowness. `/v1/price` fine but `/v1/oracle/latest` breaching points at that endpoint's path (e.g. SEP-40 contract read latency).

Mitigation:

- [ ] Confirm the breach is real (histogram agrees), not probe-host-only.
- [ ] Probe-only: investigate the probe host's network path; not a real-impact alert.
- [ ] Real: route to `api.md#stellarindex_api_latency_p95_high` for the full latency-triage flow.
- [ ] Verification: probe p95 under 200 ms for 30 min (2 consecutive passes).

False positives: first scrape after a deploy (cold Redis + Timescale buffers; absorbed by `for`), probe host CPU contention.

## stellarindex_sla_probe_p99_breach

Page. Published tail-latency target (p99 <= 500 ms) breached on the named endpoint. Otherwise the p95 twin: same diagnostics and mitigation as [p95](#stellarindex_sla_probe_p95_breach) with the 0.99 quantile.

- `stellarindex_sla_probe_latency_ms{endpoint=...,quantile="0.99"} > 500` for >= 30 min of consecutive runs.
- With p95 healthy, the tail is usually one slow code path or one heavy pair on the endpoint, not a saturated backend.
- `stellarindex_api_latency_p99_high` (real traffic, > 2 s, ticket) may or may not follow: it is a fleet-wide backstop, not the SLA line.

Step 2 of the p95 diagnosis with the 0.99 quantile:

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.99, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~"stellarindex[_-]api"}[5m])))' | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head
```

Mitigation (<= 15 min):

- [ ] Confirm real-traffic p99 on the same route agrees, not a probe-host artefact.
- [ ] Route to `api.md#stellarindex_api_latency_p95_high` for latency triage.
- [ ] Verification: probe p99 back under 500 ms for 30 min.

False positives: cold caches after a deploy (the 30 m `for` absorbs one run), probe host CPU contention.

## stellarindex_sla_probe_availability_breach

Page. Published availability target (>= 99.9 % of probe requests answered 2xx) breached on the named endpoint.

- `stellarindex_sla_probe_availability_pct{endpoint=...} < 99.9` for >= 30 min of consecutive runs.
- A run holds roughly 150 samples per endpoint, so a single non-2xx answer puts the run below 99.9 %. The `for` separates a sustained breach from one unlucky request.
- Latency and freshness may be healthy while this fires: the probe counts every 429, 5xx and client timeout as unavailable.
- `stellarindex_sla_probe_unit_failed_alert` follows on the same window; it is the umbrella, this alert names the endpoint.

Quick diagnosis (<= 5 min): verdict and one-off run per shared commands, then does real traffic agree?

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=sum by (status) (rate(http_requests_total{job=~"stellarindex[_-]api"}[15m]))' | \
  jq -r '.data.result[] | "\(.metric.status): \(.value[1])"'
```

Mitigation (<= 15 min):

- [ ] Split the failures by status. 429 on the probe only: the probe's key is missing, revoked or under-quota; fix the key, not the API.
- [ ] 5xx: route to `api.md#stellarindex_api_error_rate_critical`.
- [ ] Timeouts: route to `api.md#stellarindex_api_latency_p95_high`.
- [ ] Verification: two consecutive probe runs at >= 99.9 % on the endpoint (the alert clears on the same 30 m `for`).

False positives: probe-side 429s (F-1311; API healthy, measurement not), a deploy restart inside a run (a handful of connection refusals; one run only).

## stellarindex_sla_probe_freshness_breach

Page. Detail-page consumers see stale prices. The 30 s SLA is the DEFAULT for every freshness-bearing endpoint. `/v1/price` is the one exemption: it serves the last CLOSED bucket (ADR-0015), so its `observed_at` is structurally 30-150 s old by design. The probe holds it to a 150 s verdict bound (`defaultClosedBucketFreshTarget`, `cmd/stellarindex-sla-probe/main.go`), `/sla` publishes that bound, and the alert pages at 150 s. A sustained breach means the surface is out of date beyond even those allowances.

Expression, sustained `for: 30m`:

```promql
stellarindex_sla_probe_freshness_sec{endpoint="price"} > 150
or
stellarindex_sla_probe_freshness_sec{endpoint!="price"} > 30
```

- Read the alert's `summary` for the value; the description covers both bounds and cannot tell you which one tripped.
- The probe report shows `observed_at` on `/v1/price` (or `/v1/price/tip`) beyond its bound. Only `price` and `price-tip` publish freshness today; the third endpoint, `oracle-latest` (label on the wire, not `oracle_latest`), emits no `observed_at`. If it ever grows one it is held to 30 s, not `/v1/price`'s exemption.
- `flags.stale: true` is set on responses for the affected pair. Clients gating on it likely back off; many do not and surface the stale value as-is.

Quick diagnosis (<= 5 min). The freshness chain has three stages; bisect by which one lags:

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

Postgres fresh but Redis stale: aggregator is not writing the cache. Postgres also stale: ingestion side. Both fresh but API stale: API is reading the wrong key.

Root causes (roughly in frequency order):

1. Aggregator orchestrator down or wedged. Nobody writes the `vwap:<base>:<quote>:<window>` keys, so `/v1/price` falls back to the slower Postgres path and freshness lags as the tick gap grows.
   - Signal: `rate(stellarindex_aggregator_ticks_total[5m]) == 0`; [aggregator-silent](aggregator.md#stellarindex_aggregator_silent) fires on `stellarindex_aggregator_vwap_writes_total`.
   - Mitigation: restart the aggregator binary; investigate why it stopped.
2. Indexer lag. The dispatcher is behind on LCM consumption, so no closed buckets form.
   - Signal: [source-stopped](source-stopped.md) (`stellarindex_ingestion_source_stopped`) fires when `sum by (source) (rate(stellarindex_source_events_total[30m])) == 0` for 15m, gated on `stellarindex_source_enabled == 1` and the rule's continuous-source allowlist (binance, bitstamp, coinbase, kraken, sdex, aquarius, reflector-dex/cex/fx, redstone, coingecko). Sporadic sources (band, blend, comet, ecb, phoenix, ...) are excluded and will not page.
   - Mitigation: see `core-lag.md`.
3. CAGG refresh policy paused or lagging. `prices_1m` is not materializing recent buckets although raw trades exist.
   - Signal: `stellarindex_timescale_cagg_stale` fires too. Mitigation: see `cagg-stale.md`.
4. No trades for the pair in the tip's escalation window. Most common cause on `price-tip`, and in-contract. `computeTip` tries the caller's window (5 s), escalates to 30 s, then falls back to `PriceReader.LatestPrice` (the CLOSED bucket). A pair with no trade for 30+ s serves a 60-120 s `observed_at` on the tip surface exactly as ADR-0018 describes, and the probe correctly records it over the 30 s SLA. The tell: `price` and `price-tip` report the SAME freshness to three decimals (both serve one closed bucket). Seen on r1 2026-09-05 07:19 UTC: both read `109.156` because XLM/`fiat:USD` had a 67 s CEX trade gap (07:18:31 to 07:19:38) covering two thirds of the 30 s run.
   - Signal: bounded trade query over the run window:

     ```sh
     ssh root@136.243.90.96 "cd /tmp && runuser -u postgres -- psql -d stellarindex -X -c \"
       SELECT ts, source, quote_asset FROM trades
       WHERE ts >= now() - interval '5 minutes'
         AND base_asset IN ('native','crypto:XLM')
         AND quote_asset = 'fiat:USD'
       ORDER BY ts;\""
     ```

   - Mitigation: none on the serving side; the closed bucket IS the honest answer. A chronically quiet pair needs source coverage, not a looser threshold. Ack, and do not widen the 30 s bound: it is the published SLA.
5. No trades for the asset at all. Legitimate market quiet; the stale flag is correct.
   - Signal: trade-count panel for the pair shows zero recent rows. Ack if the asset is known-thin.

Mitigation:

- [ ] Bisect via quick diagnosis to the lagging stage.
- [ ] Route to the stage-specific runbook (aggregator / indexer / CAGG).
- [ ] "No trades in window" is honest staleness: confirm the pair is quiet and ack.
- [ ] Verification: probe `freshness_sec` under the per-endpoint bound (150 s `price`, 30 s everything else) for >= 30 min.

False positives:

- Run-duration measurement bias (fixed 2026-09-05; check the binary version first). The probe used to compute freshness as `time.Since(observed_at)` once AFTER the whole run, so every sample was charged the distance to the end of the run: median bias of `SLA_PROBE_DURATION / 2`. On r1 at the 30 s default the tip read ~15 s against a sub-second truth; at the 120 s duration `sla-probe.sh` recommends for memory-pressured hosts it would read ~60 s and page permanently. Freshness is now anchored to each sample's own receipt instant (`probeSample.receivedAt`). If the deployed binary predates the fix, subtract `run_duration_seconds / 2` from the reading.
- Newly-listed asset with low volume. Consider a "thin-pair allowlist" if it persists.
- Maintenance windows with the indexer or aggregator intentionally paused. Pre-silence the alert.

## stellarindex_sla_probe_unit_failed_alert

Ticket (P3), `stellarindex_sla_probe_unit_failed > 0` for >= 30 min. Umbrella signal: at least one SLA is breached. The per-breach alerts ([p95](#stellarindex_sla_probe_p95_breach), [p99](#stellarindex_sla_probe_p99_breach), [availability](#stellarindex_sla_probe_availability_breach), [freshness](#stellarindex_sla_probe_freshness_breach)) are the "fix THIS thing" pages on the same 30 m `for`; this is the "one of them is firing somewhere" ticket. MTTR depends on the underlying breach.

- The latest report carries a non-empty `failed_reasons` array.
- Often (not always) accompanied by a per-breach alert.

Quick diagnosis (<= 5 min): reproduce the verdict locally on r1 (the wrapper's env supplies the defaults):

```sh
ssh root@136.243.90.96
set -a; source /etc/default/stellarindex-healthchecks; set +a
/usr/local/bin/stellarindex-sla-probe -base-url "${SLA_PROBE_BASE_URL:-http://localhost:3000/v1}" \
  -pair "${SLA_PROBE_PAIR:-native,fiat:USD}" -duration 30s -concurrency 1 \
  -report-format json | jq -r '.failed_reasons[]'
```

Or read the Healthchecks.io check's last ping body. Output is one of:

```
price: p95=215.3ms > target 200.0ms
price: p99=523.1ms > target 500.0ms
healthz: availability=98.50% < target 99.90%
price-tip: freshness=42.1s > target 30.0s
price: freshness=163.2s > target 150.0s
```

The 30 s freshness target applies to `price-tip`; `price` is measured against the 150 s closed-bucket target, so a `price: ... > target 30.0s` line cannot occur.

Mitigation:

- [ ] Read `failed_reasons` to identify the breach kind.
- [ ] Route: p95 / p99 to [p95](#stellarindex_sla_probe_p95_breach); freshness to [freshness](#stellarindex_sla_probe_freshness_breach); availability to [availability](#stellarindex_sla_probe_availability_breach) (the probe's availability is the 2xx-success rate, the same signal as `api.md#stellarindex_api_error_rate_critical`).
- [ ] Verification: `unit_failed` at 0 for >= 30 min.

Why it exists alongside the per-breach alerts: it catches a new endpoint or pair-specific threshold not yet wired into a per-breach rule. If it fires without a per-breach companion, add the per-breach rule. (p99 and availability now have their own rules.)

False positives: none expected; the verdict is deterministic from measured values. A firing with no real breach is a bug in `cmd/stellarindex-sla-probe/main.go::computeVerdict`.

## stellarindex_sla_probe_stale

Page (P2), `for: 5m`. The SLA-evidence trail is lost; the API itself may be fine, we just cannot prove it. MTTR 15 min.

Expression, TWO branches:

```promql
(time() - stellarindex_sla_probe_last_pass_timestamp) > (90 * 60)
or
absent_over_time(stellarindex_sla_probe_last_pass_timestamp[90m])
```

- 90 min = 6 missed passes of the 15-min cadence.
- OBS-2: the probe writes `last_pass_timestamp` only on a pass and rewrites the whole textfile each run, so a failing run drops the series. Every-run-failed makes the series ABSENT, not old; the `absent_over_time` branch is what catches it.
- Either the timer is not running, or every recent run failed (which also fires `_unit_failed_alert`).
- Do not confuse the live `stellarindex-sla-probe.timer` with the retired duplicate GO-stack `sla-probe.timer`; ansible actively removes the latter.

Quick diagnosis (<= 5 min):

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

Root causes:

1. Timer disabled. An operator stopped it for maintenance and forgot; `systemctl status` shows `inactive`. Fix: `sudo systemctl enable --now stellarindex-sla-probe.timer`.
2. Service unit failing every run; shows in journald. Check the entries and handle as [unit_failed](#stellarindex_sla_probe_unit_failed_alert).
3. node_exporter not scraping the textfile_collector dir. Under OBS-2 the signal is ambiguous; disambiguate with `grep last_pass /var/lib/node_exporter/textfile_collector/sla_probe.prom`:
   - Line present in the file but series absent in Prometheus: node_exporter / textfile-collector problem; confirm `--collector.textfile.directory` points at the right path.
   - Line absent from a FRESH file: the probe fails every run (check `stellarindex_sla_probe_unit_failed=1` in the same file); go to [unit_failed](#stellarindex_sla_probe_unit_failed_alert).
4. `SLA_PROBE_TEXTFILE_OUTPUT` explicitly blanked. The wrapper DEFAULTS the path (`configs/healthchecks/sla-probe.sh:60`), so an absent var still writes; only an emptied `SLA_PROBE_TEXTFILE_OUTPUT=` in `/etc/default/stellarindex-healthchecks` disables it. (`TEXTFILE_OUTPUT` belongs to the supply-snapshot unit.) The other file-missing cause is the `ReadWritePaths`/EROFS class the unit documents: the service cannot write outside its allowed paths.
   - Signal: the file does not exist at all. Fix: un-blank the var (or fix `ReadWritePaths`); reload the service.

Mitigation:

- [ ] Walk the diagnostics; find the silent stage.
- [ ] Apply the matching root-cause fix.
- [ ] Force a run (`systemctl start stellarindex-sla-probe.service`) and confirm `last_pass_timestamp` updates.
- [ ] Verification: alert clears within 5 min of a passing run reaching node_exporter.

False positive: fresh deploy of the probe. The series is absent (an absent series is an empty vector, so the first branch cannot fire); the `absent_over_time` branch fires until the first PASSING run lands, plus `for: 5m`. Force a run rather than silencing.

## Related

- `api.md#stellarindex_api_latency_p95_high` - underlying latency triage.
- `api.md#stellarindex_api_error_rate_critical` - server-error triage.
- `slo-availability-burn-fast.md` - real-traffic availability burn.
- `cagg-stale.md`, `core-lag.md`, `aggregator.md#stellarindex_aggregator_silent` - freshness chain stages.
- ADR-0015 - closed-bucket-only serving that makes `/v1/price` structurally 30-150 s old.
- The service freshness SLA - the 30 s spec (tip-of-chain surface).
