---
title: Runbook — aggregator alerts
last_verified: 2026-10-05
status: draft
---
# Aggregator alerts

## At a glance

Aggregator, FX snap and VWAP alerts; each section below is one alert.

Alert group `stellarindex.aggregator` (`configs/prometheus/rules.r1/aggregator.yml`; twin `deploy/monitoring/rules/aggregator.yml`). Aggregator metrics `:9465`, indexer `:9464`; r1 is `ssh root@136.243.90.96`.

## stellarindex_aggregator_bootstrap_cap_reengaged

P3 (ticket). Expr: `count((stellarindex_aggregator_bootstrap_capped == 1) and (last_over_time(stellarindex_aggregator_bootstrap_capped[2d] offset 1h) == 0)) >= 3`, `for: 10m`. Three or more pairs released from the ADR-0019 bootstrap cap are pinned at confidence 0.5 again (`/v1/price` shows `bootstrap_capped: true`); prices still serve.

The cap releases at 28.5 days of 1-minute buckets in the 30-day baseline and re-caps below 27; several pairs together lost the same minutes.

### Check

Graph `stellarindex_aggregator_baseline_density_days` for the capped pairs (a step of N days = N x 1,440 missing buckets), then count buckets per day for one pair:

```sql
SELECT date_trunc('day', bucket) AS day, count(*) AS buckets
FROM prices_1m
WHERE base_asset = '<base>' AND quote_asset = '<quote>'
  AND bucket > now() - INTERVAL '30 days'
GROUP BY 1 ORDER BY 1;
```

Trades present for the short day: aggregate never materialised. Trades missing: ingestion gap.

### Likely causes

- Ingestion outage or aggregator outage.
- Three thin pairs hovering at the gate (no common short day): false positive.

### Fix

1. Buckets missing, trades present: `CALL refresh_continuous_aggregate('prices_1m', '<from>', '<to>');`. The cap releases after the next baseline refresh and confidence compute.
2. Trades missing: repair ingestion first ([`ingestion.md#stellarindex_ingestion_all_sources_stopped`](ingestion.md#stellarindex_ingestion_all_sources_stopped) or `#stellarindex_ingestion_source_stopped`), then refresh.
3. Unrepairable gap: the cap stays until it leaves the 30-day window.

Related: [`anomaly.md#stellarindex_anomaly_freeze_engaged`](anomaly.md#stellarindex_anomaly_freeze_engaged).

## stellarindex_aggregator_cache_write_errors

Page. Expr: `sum(rate(stellarindex_aggregator_vwap_cache_write_errors_total[5m])) > 0`, `for: 2m`. Corroborates `stellarindex_redis_writes_blocked`. VWAP cache writes fail, so `/v1/price` on rewritten / triangulated / stablecoin-proxy pairs 404s; pairs served from `prices_1m` still work and `flags.stale` does not fire. May escalate to [`api.md`](api.md#stellarindex_ratelimit_fail_closed) (whole-API 503).

Usual cause: root FS full, so Redis refuses writes (`MISCONF`). Aggregator logs WARN `refresh failed` every tick.

### Check

```sh
ssh root@136.243.90.96 'redis-cli SET test:probe x'  # -> MISCONF Redis is configured to save RDB...
ssh root@136.243.90.96 'df -h /'
ssh root@136.243.90.96 'tail -20 /var/log/redis/redis-server.log'
```

### Likely causes

- Root FS full (journal, syslog archives, WASM-audit stderr captures, Postgres logs).
- Redis down or out of memory instead: [`cache.md#stellarindex_redis_master_down`](cache.md#stellarindex_redis_master_down), [`cache.md#stellarindex_redis_memory_saturated`](cache.md#stellarindex_redis_memory_saturated).

### Fix

1. Free disk on `/` on r1 (target under 80% used, 10 GB+ free), lowest-risk first:

```sh
journalctl --vacuum-size=200M
truncate -s 0 /var/log/syslog.1
pgrep -af 'stellarindex-ops wasm-history'   # empty = no walk running; -f is required
rm -f /var/log/wasm-audit/*.stderr /var/log/wasm-history-*.stderr
ls -la /var/log/postgresql/
df -h /
```

2. Unblock Redis (a successful BGSAVE clears the flag):

```sh
ssh root@136.243.90.96 'redis-cli BGSAVE'      # -> Background saving started
ssh root@136.243.90.96 'redis-cli LASTSAVE'    # timestamp moves to ~now
ssh root@136.243.90.96 'redis-cli SET test:probe ok && redis-cli GET test:probe && redis-cli DEL test:probe'  # -> OK / ok / 1
```

3. Confirm: `journalctl -u stellarindex-aggregator --since "30 seconds ago" -o cat | grep -c "refresh failed"` returns 0, and `/v1/price` returns a recent `observed_at`.
4. Fix the disk per [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full).

Escalate: disk will not free, or Redis still rejects writes after BGSAVE. Past incident: `internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`.

## stellarindex_aggregator_class_drop_spike

P3. Expr: `sum(rate(stellarindex_aggregator_dropped_trades_total{reason="class"}[10m]))` above 10x its `offset 1h` baseline, `for: 15m`. The class filter is dropping trades; most often a new venue not yet in `external.Registry` hits the fail-closed `IncludeInVWAP=false` fallback. Ingest still records rows.

### Check

```sh
ssh root@136.243.90.96
curl -fs http://localhost:9464/metrics \
  | grep '^stellarindex_source_events_total{' | sort -t'"' -k2
grep -E '"[a-z][a-z0-9_-]+":\s*\{' \
  internal/sources/external/registry.go
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT source, COUNT(*) AS rows, MIN(timestamp) AS first_seen
   FROM trades
   WHERE timestamp > now() - interval '1 hour'
   GROUP BY source
   ORDER BY first_seen DESC
   LIMIT 20;"
```


### Likely causes

- Net-new venue not yet registered.
- Unintended source name (typo, renamed connector, dev binary; check `stellarindex_source_enabled`).
- Aggregator/oracle source volume spike (expected; dropped from VWAP).

### Fix

1. New venue: add a one-line `external.Registry` entry under the right `Class*` constant (`ClassExchange + IncludeInVWAP=true` once classified; paid-tier aggregator/oracle stays false).
2. Rogue source name: find the process and roll back; its trades stay valid.
3. Existing aggregator/oracle spike: document the cause and silence; no code change.
4. Verify the `reason="class"` rate returns to baseline (after a registry update `vwap_writes_total` also rises).

See [`aggregation-plan.md`](../../architecture/aggregation-plan.md).

## stellarindex_aggregator_fx_snap_fallback_dominant

P3, `for: 30m`:

```promql
sum(rate(stellarindex_aggregator_fx_snap_fallback_total[15m]))
/
clamp_min(
  sum(rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])),
  1
) > 0.5
```

Chained-fiat triangulation (e.g. XLM/EUR) lacks the bucket-end FX snap required by ADR-0018. On r1 all four default chains (XLM/EUR, XLM/GBP, ETH/GBP, BTC/GBP) use fiat/fiat legs with no VWAP cache fallback, so a snap miss is `outcome="missing_leg"` and the chain does NOT publish; `/v1/price` chained-fiat responses stop updating. Blind spot: with `ok` at zero the ratio cannot reach 0.5, so a total drought is [chains_dry](#stellarindex_aggregator_triangulation_chains_dry).

FX source: the `massive` forex worker in the API binary (key `MASSIVE_API_KEY`) writes hourly rows to `fx_quotes`; `FXQuoteAtOrBefore` reads them with a 7-day lookback.

### Check

```sh
ssh root@136.243.90.96
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_fx_snap_fallback_total'
curl -fs http://localhost:3000/metrics | grep external_fx_last_quote_unix
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT ticker, max(bucket) FROM fx_quotes WHERE ticker IN ('EUR','GBP') GROUP BY 1;"
```

### Likely causes

- `fx_quotes` fresh (under 2 h): snap-rule logic bug with healthy FX; check recent commits to the snap path, `FXQuoteAtOrBefore`, and that API and aggregator agree on `external.FXSources()`.
- Stale 2 h - 7 d or older than 7 d: worker lagging; `stellarindex_external_fx_feed_stale` should also fire ([external-pollers.md](external-pollers.md#stellarindex_external_fx_feed_stale)). Over 7 d, chains are dry.
- No rows: worker never ran; check `MASSIVE_API_KEY` in `/etc/default/stellarindex` (worker is in the API binary, not the indexer).

### Fix

1. Feed stale or absent: usually an expired or missing `MASSIVE_API_KEY` or upstream degradation; fix the env, restart the API service.
2. Chains stopped publishing: handle as `chains_dry`. Verify the fallback rate nears zero within 30 min.

Escalate: attach the 1 h metric range and the `missing_leg` rate.

## stellarindex_aggregator_outlier_storm

P3, `for: 15m`. Expr: `max by (pair)(stellarindex_aggregator_venue_vwap{window="5m"}) / min by (pair)(...) - 1 > 0.01` with 2 or more venue series: venues disagree by over 1% on 5m VWAPs. One venue is stale, thin, mis-decoding amounts, or a stablecoin leg is skewed. The published price is protected (time-local filtered VWAP); this is a source-health signal. Check `flags.divergence_warning` and `stellarindex_divergence_max_abs_fraction` for price impact.

### Check

```sh
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_venue_vwap{' | grep 'window="5m"' | sort
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_window_trades{' | grep 'window="24h"' | sort
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_window_base_volume{' | sort
psql -d stellarindex -c \
  "SELECT pair, source, COUNT(*) AS rows
   FROM trades
   WHERE timestamp > now() - interval '15 minutes'
   GROUP BY pair, source ORDER BY 3 DESC LIMIT 10;"
```

### Likely causes

- Trade rate up, many pairs: one venue lagging a real market-wide move (stale feed, throttled poller).
- Trade rate up, one pair: pair-specific dislocation or depeg; consider pausing the stablecoin proxy for that quote.
- Trade rate normal, one pair and source: single connector misbehaving (amount-decimal regression); also an SDEX token-farm spam wave (one issuer self-trading, 25-37% gaps, dust volumes; a single venue cannot disagree with itself, so [trim_fraction](#stellarindex_aggregator_outlier_trim_fraction) fires instead and the filter is doing its job).

### Fix

1. Real market event: leave the filter alone.
2. Connector regression: find the source via `stellarindex_source_events_total` x `stellarindex_source_decode_errors_total` and the deploy diff. Set `[external.<venue>] enabled = false` and add it to `aggregate.excluded_sources` (drops stored trades from VWAP at read time); restart the aggregator.
3. Mis-calibration (`trim_fraction` sustained over 1 h, neither case above): raise `aggregate.outlier_sigma_threshold` 4.0 to 5.0 or 6.0 while RCA continues. An agreed move being trimmed is a filter bug: capture pair and window and file it.
4. Verify venue spread (`max/min - 1`) is under 1%.

False positive: USDT-quoted vs USD-quoted venues differ by the USDT/USD basis (over 1% sustained is a real depeg).

Often co-fires with [silent](#stellarindex_aggregator_silent).

## stellarindex_aggregator_outlier_trim_fraction

P3, `for: 30m`. Expr: `window="24h"` and `1 - window_trades{stage="outlier"} / window_trades{stage="class"} > 0.2` with class count at least 20. The time-local filter rejects over 20% of one pair's 24h window: a spam / wash / dust wave, typically single-venue (invisible to [outlier_storm](#stellarindex_aggregator_outlier_storm)). The published price stays protected; the harm is a source wholesale-rejected or a thin survivor sample.

Check and Fix: use the outlier_storm checks 2, 3 and 4 (`window_trades` per stage, trade rate by pair/source, `topk` on the drop counter), then the single-issuer pattern in `trades`. A wave still self-validates when it is the majority of the 24h window, within the ~4% anchor tolerance of the honest level, or a random walk in steps of 4% or less. Verify the ratio returns under 0.2; if sustained over 1 h with no spam or connector cause, raise `aggregate.outlier_sigma_threshold`.

## stellarindex_aggregator_outlier_volume_trim_fraction

P3, `for: 15m`. Expr: on any window, `1 - window_base_volume{stage="outlier"} / window_base_volume{stage="class"} > 0.2` with class volume above 0. The filter removed over 20% of a pair's base volume. A value of exactly 1 with `window_trades{stage="class"} > 0` means the window is WITHHELD (contested, no price published): a wash-over-block shape.

Check: read `window_base_volume{stage}` (outlier_storm check 2b) beside `window_trades`: many prints but little volume removed is dust; few prints and most volume removed is a large print outside the band, or wash prints outvoting an honest block. Then the trade rate and `topk` as in [outlier_storm](#stellarindex_aggregator_outlier_storm). Fix: withheld windows clear once the contested prints leave the window or the wash source is added to `aggregate.excluded_sources`.

## stellarindex_aggregator_silent

P1 (page), `for: 5m`. Expr: `sum(rate(stellarindex_aggregator_vwap_writes_total[5m])) == 0 OR absent_over_time(stellarindex_aggregator_vwap_writes_total[10m]) == 1`. Zero VWAP cache writes for 5+ min: `/v1/price` serves staler cached VWAPs (`flags.stale` flips) and rewritten / triangulated pairs 404 as key TTLs lapse. `/v1/vwap` is unaffected (scans raw trades on-query).

### Check

```sh
ssh root@136.243.90.96
systemctl status stellarindex-aggregator
journalctl -u stellarindex-aggregator -n 50 --no-pager
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_aggregator_(ticks_total|empty_windows|dropped_trades|vwap_writes)'
redis-cli PING
redis-cli SET _aggregator_probe "$(date -Iseconds)" EX 60
runuser -u postgres -- psql -d stellarindex -c "SELECT pair, COUNT(*) FROM trades WHERE timestamp > now() - interval '5 minutes' GROUP BY pair ORDER BY 2 DESC LIMIT 10;"
```

### Likely causes

- `ticks_total{outcome="ok"}` advancing, `vwap_writes_total` flat: every (pair, window) lands in the empty-window branch; check `empty_windows_total` and Timescale trade volume.
- Only `ticks_total{outcome="error"}` advancing: refresh failures (`refresh failed` in journal); Timescale or Redis side.
- `ticks_total` flat: tick loop stuck.
- Redis down or `MISCONF`; there is no standby (single local redis-server).

### Fix

1. Redis: fix the local redis-server and restart the unit; for `MISCONF` free disk per [cache_write_errors](#stellarindex_aggregator_cache_write_errors). The aggregator retries next tick.
2. Wedged loop: `systemctl restart stellarindex-aggregator`; writes resume within `interval_seconds` (default 30 s).
3. Empty windows with trades present: check whether `enable_stablecoin_fiat_proxy` is needed (a `XLM/fiat:USD` pair without expansion matches only direct FX trades).
4. Verify `vwap_writes_total` advances.

False positive: aggregator not deployed in this environment. Escalate: capture `journalctl -u stellarindex-aggregator --since='15 minutes ago'`, metrics before and after, and `cfg.Aggregate.*`.

## stellarindex_aggregator_triangulation_chains_dry

P3, `for: 30m`. Expr: `sum(rate(stellarindex_aggregator_triangulations_total{outcome="missing_leg"}[15m])) > 0 and sum(rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])) == 0`. The `triangulation_agreement` factor contributes nothing, so thin pairs (XLM/GBP, XLM/EUR, ETH/GBP) lose one independent check (`triangulation_checked=false`). Served prices are unaffected.

Default chains: XLM/EUR, XLM/GBP, ETH/GBP, BTC/GBP. No `fx_quotes` row in the 7-day lookback means `missing_leg` and the chain does not publish. Partial degradation: [fx_snap_fallback_dominant](#stellarindex_aggregator_fx_snap_fallback_dominant).

### Check

```sh
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_triangulations_total'
curl -fs http://localhost:3000/metrics \
  | grep '^stellarindex_external_fx_last_quote_unix'
psql -d stellarindex -c \
  "SELECT ticker, source, max(bucket_day) AS latest, now()::date - max(bucket_day) AS days_old
     FROM fx_quotes
    WHERE ticker IN ('EUR','GBP')
    GROUP BY ticker, source
    ORDER BY latest DESC;"
```

### Likely causes

- Newest `fx_quotes` row older than 7 days or table empty: forex worker dead or never started in the API binary.
- Chain references a cross with no feed.

### Fix

1. Dry FX feed: check the `massive` worker is up and `stellarindex_external_fx_last_quote_unix{source="massive"}` advances, then the upstream credential or quota. Chains recover on the next tick; no aggregator restart.
2. Cross with no feed: add it to the forex worker's ticker list or drop the chain from `aggregate.triangulations` (chains come from the ansible template, not `config.Default()`).
3. Crypto leg missing: confirm the deep USD pair (e.g. XLM/USD) publishes; else see [silent](#stellarindex_aggregator_silent).


## stellarindex_asset_character_rollup_failing

P3 (informational), `for: 5m`. Expr: `sum(increase(stellarindex_asset_character_rollup_sweeps_total{outcome="refresh_error"}[13h])) > 0 unless sum(increase(stellarindex_asset_character_rollup_sweeps_total{outcome="ok"}[13h])) > 0` (two failed 6 h sweeps in a row). Last-good `volume_character` keeps serving.

### Check

```sh
curl -s localhost:9464/metrics | grep stellarindex_asset_character_rollup_sweeps_total
journalctl -u stellarindex-aggregator --since -12h | grep -i "asset-character rollup"
sudo -u postgres psql stellarindex -c '\d asset_volume_character'
sudo -u postgres psql stellarindex -c \
  'SELECT count(*), max(computed_at) FROM asset_volume_character;'
```

### Likely causes

- Table missing: migration 0149 did not apply.
- Postgres down, lock contention, or statement timeout.

### Fix

1. Missing table: run the migrator (deploy.yml auto-applies).
2. Postgres trouble: follow the storage runbook. No manual catch-up; the worker retries every 6 h (a restart re-runs the first sweep after 30 min).


## stellarindex_asset_volume_rollup_failing

P3 (informational), `for: 30m`. Expr: `sum(rate(stellarindex_asset_volume_rollup_sweeps_total{outcome="refresh_error"}[15m])) > 0` (sweeps every ~2 min). `/v1/assets` `volume_24h_usd` freezes at its last-good value.

### Check

```sh
curl -s localhost:9464/metrics | grep stellarindex_asset_volume_rollup_sweeps_total
journalctl -u stellarindex-aggregator --since -30min | grep -i "asset-volume rollup"
sudo -u postgres psql stellarindex -c '\d asset_volume_24h'
sudo -u postgres psql stellarindex -c \
  'SELECT count(*), max(computed_at) FROM asset_volume_24h;'
```

### Likely causes

- Postgres reachability or served-tier lock/IO pressure (check what else fired).
- Migration 0087 not applied.

### Fix

1. Missing table: run the migrator (deploy.yml auto-applies; manual: `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up`).
2. Postgres down or locked: follow the storage runbook. The alert clears on the next good sweep; the full trailing-24h volume is recomputed, so no catch-up step.


## stellarindex_change_summary_stale

P3, `for: 15m`. Expr: `(time() - stellarindex_change_summary_last_success_unix{job="stellarindex-aggregator"}) > 1800 and stellarindex_change_summary_last_success_unix{job="stellarindex-aggregator"} > 0`. No pass upserted any entity for 30 min (`stellarindex_change_summary_passes_total{outcome="failed"}` usually rising); explorer delta strips serve frozen rows. `partial` passes are normal.

### Check

```sh
systemctl is-active stellarindex-aggregator
journalctl -u stellarindex-aggregator --since "-30min" | grep 'change-summary pass had failures'
```

The warn carries `first_err`.

### Likely causes

- `no closed observations in window` for every entity: `prices_1m` is not advancing (check price CAGG refresh and ingest freshness).
- Postgres error: reachability, lock contention, or missing `change_summary_5m` migration.
- Aggregator down: [silent](#stellarindex_aggregator_silent).

### Fix

1. Fix the cause; the next 5-minute pass upserts every entity.
2. Verify `stellarindex_change_summary_last_success_unix` advances within 5 min.

To clear a bad all-time high or low (`change_summary_5m` ratchets `ath`/`atl` with `GREATEST`/`LEAST`, so only deleting the row clears it; the worker rebuilds from 30 days within 5 min; dry run unless `-write`):

```sh
stellarindex-ops change-summary-reset -config /etc/stellarindex.toml -entity-type coin -entity-id crypto:XLM -write
```

## stellarindex_decimals_guard_sweep_stale

P3, `for: 15m`. Expr: `(time() - stellarindex_decimals_guard_sweep_last_success_unix{job="stellarindex-aggregator"}) > 2700 and stellarindex_decimals_guard_sweep_last_success_unix{job="stellarindex-aggregator"} > 0`. The decimals guard (`internal/decimalsguard`, 15 min sweep) has not completed a pass in 45 min: never armed (ClickHouse dial retrying or startup Backfill) or wedged. A gauge of 0 means deliberately disabled and is excluded.

### Check

```sh
journalctl -u stellarindex-aggregator | grep decimals-guard
```

Look for `decimals-guard: ClickHouse decimals resolver unavailable` / `decimals-guard started` / `decimals-guard exited with error`, and check ClickHouse reachability.

### Fix

1. Restart the aggregator if wedged.
2. Rest: [dex.md#dex-nonstandard-decimals](dex.md#dex-nonstandard-decimals).

## stellarindex_nonstandard_decimals_correction_failing

P3 ticket, `for: 5m`. Fires on any of: increase over 15 m of `stellarindex_nonstandard_decimals_cache_refresh_failures_total` (API cache refresh against Postgres failing), `stellarindex_price_serve_declined_nonstandard_decimals_total` (should be permanently zero; regression signal), or `stellarindex_nonstandard_decimals_lockstep_mismatch_total{site,asset}` (lake `decimals()` and the `nonstandard_decimals_assets` projection disagree for `asset`; only this arm names the token). New non-7-decimal tokens may serve raw, skewed ratios.

### Check

```sh
curl -s http://localhost:9465/metrics | grep -E 'nonstandard_decimals_cache_refresh_failures_total|price_serve_declined_nonstandard_decimals_total'
journalctl -u stellarindex-aggregator | grep decimals-guard
```

For a lockstep hit read the ERROR `decimals-guard: nonstandard_decimals_assets row DISAGREES with the lake` for `persisted_decimals` vs `lake_decimals`.

### Fix

Mitigation, hand-seeding and the lockstep `site` meanings: [dex.md#dex-nonstandard-decimals](dex.md#dex-nonstandard-decimals).

## stellarindex_customer_webhook_fanout_failing

P3, `for: 5m`. Expr: `sum by (event_type, reason) (increase(stellarindex_customer_webhook_fanout_failures_total[1h])) > 0`; one occurrence is worth a ticket. A subscriber got no delivery row written, so nothing retries and the event is lost to them until re-emitted. Delivery-attempt failures after a row exists are [`api.md#stellarindex_customer_webhook_delivery_failing`](api.md#stellarindex_customer_webhook_delivery_failing). Fan-out failure means no delivery row was written, so nothing retries; delivery attempts that do exist retry on a 15-attempt budget whose last retry lands ~4–8 h after the event.

`reason`: `enqueue` (one subscriber's INSERT failed), `list_subscribers` (every subscriber of that event type), `invalid_payload` (producer code bug).

### Check

```promql
sum by (event_type, reason) (
  increase(stellarindex_customer_webhook_fanout_failures_total[1h])
)
```

Each increment pairs with an ERROR naming the lost event:

```sh
journalctl -u stellarindex-aggregator --since '-2h' \
  | grep -E 'customerwebhook.fanout|fan-out lost'
```

For `enqueue` / `list_subscribers` check Postgres (connections, disk, statement timeout, grants):

```sh
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT count(*) FROM webhook_deliveries WHERE created_at > now() - interval '\''1 hour'\'';
"'
```

### Likely causes

- Postgres saturated, disk full, statement timeout, or grants (`enqueue`, `list_subscribers`).
- Producer marshalling bug (`invalid_payload`).

### Fix

1. Fix the store first.
2. Find losses from the source of truth and compare the window with `webhook_deliveries` (expected rows that are missing are the losses): `anomaly.freeze` from `freeze_events`; `divergence.firing` from `divergence_runs`; `incident.sev1` / `incident.resolved` from incident markdown under `deploy/comms/`; `price.alert` from `price_alerts`.
3. Re-emit incidents with `stellarindex-ops emit-incident -slug <slug> -event <sev1|resolved> -write` (non-zero exit if the fan-out loses anything). Freeze and divergence have no re-emit command: contact the customer directly.
4. `invalid_payload`: find the call site from the log line and fix the marshalling. Do not make the fan-out blocking (it would stall the price pipeline).

See also [admin-audit-write-failing](api.md#stellarindex_admin_audit_write_failing).

## stellarindex_protocol_events_rollup_failing

P3 (informational), `for: 30m`. Expr: `sum(rate(stellarindex_protocol_events_rollup_sweeps_total{outcome="refresh_error"}[15m])) > 0` (sweeps every ~2 min). `events_24h` on `/v1/protocols` freezes.

### Check

```sh
curl -s localhost:9464/metrics | grep stellarindex_protocol_events_rollup_sweeps_total
journalctl -u stellarindex-aggregator --since -30min | grep -i "protocol-events rollup"
sudo -u postgres psql stellarindex -c '\d protocol_events_24h'
sudo -u postgres psql stellarindex -c 'SELECT source, events_24h FROM protocol_events_24h ORDER BY events_24h DESC;'
```

### Likely causes

- Postgres reachability or served-tier lock pressure (check what else fired).
- Migration 0086 not applied.

### Fix

1. Missing table: run `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up` (deploy.yml auto-applies).
2. Postgres down or locked: follow the storage runbook. The next good sweep clears the alert; the full trailing-24h census is recomputed.

## Related

- [Alerts catalogue](../alerts-catalog.md)
