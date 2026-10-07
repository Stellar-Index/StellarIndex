---
title: Runbook — aggregator alerts
last_verified: 2026-10-05
status: draft
---
# Aggregator alerts

Runbooks for the `stellarindex.aggregator` alert group (`configs/prometheus/rules.r1/aggregator.yml`; multi-host twin in `deploy/monitoring/rules/aggregator.yml`). One section per alert. Aggregator metrics are on `:9465`, indexer metrics on `:9464`; r1 is `ssh root@136.243.90.96`.

## At a glance

- [`stellarindex_aggregator_bootstrap_cap_reengaged`](#stellarindex_aggregator_bootstrap_cap_reengaged)
- [`stellarindex_aggregator_cache_write_errors`](#stellarindex_aggregator_cache_write_errors)
- [`stellarindex_aggregator_class_drop_spike`](#stellarindex_aggregator_class_drop_spike)
- [`stellarindex_aggregator_fx_snap_fallback_dominant`](#stellarindex_aggregator_fx_snap_fallback_dominant)
- [`stellarindex_aggregator_outlier_storm`](#stellarindex_aggregator_outlier_storm)
- [`stellarindex_aggregator_outlier_trim_fraction`](#stellarindex_aggregator_outlier_trim_fraction)
- [`stellarindex_aggregator_outlier_volume_trim_fraction`](#stellarindex_aggregator_outlier_volume_trim_fraction)
- [`stellarindex_aggregator_silent`](#stellarindex_aggregator_silent)
- [`stellarindex_aggregator_triangulation_chains_dry`](#stellarindex_aggregator_triangulation_chains_dry)
- [`stellarindex_asset_character_rollup_failing`](#stellarindex_asset_character_rollup_failing)
- [`stellarindex_asset_volume_rollup_failing`](#stellarindex_asset_volume_rollup_failing)
- [`stellarindex_change_summary_stale`](#stellarindex_change_summary_stale)
- [`stellarindex_customer_webhook_fanout_failing`](#stellarindex_customer_webhook_fanout_failing)
- [`stellarindex_decimals_guard_sweep_stale`](#stellarindex_decimals_guard_sweep_stale)
- [`stellarindex_nonstandard_decimals_correction_failing`](#stellarindex_nonstandard_decimals_correction_failing)
- [`stellarindex_protocol_events_rollup_failing`](#stellarindex_protocol_events_rollup_failing)

## stellarindex_aggregator_bootstrap_cap_reengaged

**Severity:** P3 (ticket). **Trigger:** `count((stellarindex_aggregator_bootstrap_capped == 1) and (last_over_time(stellarindex_aggregator_bootstrap_capped[2d] offset 1h) == 0)) >= 3`, `for: 10m`; needs 3+ pairs re-capping in the same hour, each compared with its last reported state up to 2 days back, so a re-cap that arrives with a restart still counts. **MTTR:** 1-4 h to repair the gap, else up to 30 days for it to roll out.

**Impact:** 3+ pairs released from the ADR-0019 bootstrap cap are pinned at confidence 0.5 again. Prices still serve; `confidence` and the anomaly-freeze inputs drop, and `confidence_factors.bootstrap_capped` is `true` on `/v1/price`.

**Mechanism:** the cap releases when a pair's 30-day baseline holds >= 28.5 days-equivalent of 1-minute buckets (`BootstrapDensityDays`, `internal/aggregate/confidence/score.go`; density = `prices_1m` bucket count over 30 d / 1,440, refreshed about hourly). Once released it re-caps only below 27 (`BootstrapReengageDensityDays`), so dense pairs (~29.8) absorb ~67 h of missing minutes. Gate state is in aggregator memory: after a restart every pair must clear 28.5 again, so a restart after ~30 h of gap re-caps them. Several pairs together means their windows lost the same minutes: ingestion outage, aggregator outage, or trades landing after `prices_1m`'s 5-minute refresh lag and never materialised.

**Symptoms:** `stellarindex_aggregator_bootstrap_capped{pair}` 0 -> 1 on several major pairs; `stellarindex_aggregator_baseline_density_days{pair}` below 27 (or below 28.5 across a restart); `/v1/price` shows `confidence` exactly 0.5 with `bootstrap_capped: true`.

**Diagnose:**
1. Graph `stellarindex_aggregator_baseline_density_days` for the capped pairs over the last day. A step of N days-equivalent = N x 1,440 missing buckets.
2. Count `prices_1m` buckets per day for one affected pair; a day well short of 1,440 (for a pair that trades every minute) is the gap.

```sql
SELECT date_trunc('day', bucket) AS day, count(*) AS buckets
FROM prices_1m
WHERE base_asset = '<base>' AND quote_asset = '<quote>'
  AND bucket > now() - INTERVAL '30 days'
GROUP BY 1 ORDER BY 1;
```

3. Compare with `trades` for the same day: trades present but buckets missing = aggregate never materialised; trades missing too = ingestion gap.

**Fix:**
- Buckets missing, trades present: `CALL refresh_continuous_aggregate('prices_1m', '<from>', '<to>');`. The next baseline refresh restores density and the cap releases on the following confidence compute.
- Trades missing: repair ingestion first (`ingestion.md#stellarindex_ingestion_all_sources_stopped` or `ingestion.md#stellarindex_ingestion_source_stopped`), then refresh as above.
- Gap unrepairable: the cap stays until the gap leaves the 30-day window; record the expected release date in the ticket.

**False positive:** three thin pairs hovering at the gate flipping in the same hour; per-day bucket counts show no common short day.

**See also:** `anomaly.md#stellarindex_anomaly_freeze_engaged`; `docs/adr/0019-anomaly-response-and-confidence-scoring.md`; `docs/reference/metrics/README.md` § `stellarindex_aggregator_baseline_density_days`.

## stellarindex_aggregator_cache_write_errors

**Severity:** page. **Trigger:** `sum(rate(stellarindex_aggregator_vwap_cache_write_errors_total[5m])) > 0`, `for: 2m`. **MTTR:** 5-10 min once root cause is confirmed.

**Role:** corroborating alert. The primary detector is `stellarindex_redis_writes_blocked` (`storage.yml`, `redis_rdb_last_bgsave_status == 0`, `for: 60s`), which fires on Redis's own bgsave status. This alert cannot fire while the aggregator is idle (no attempted write, no counted error). Escalation: `stellarindex_ratelimit_fail_closed` (page, `api.yml`) once the rate limiter's own Redis calls fail past its 30 s fail-open dwell, after which the WHOLE API 503s (see `api.md#stellarindex_ratelimit_fail_closed`).

**Impact:** VWAP cache writes fail, so `/v1/price` on rewritten/triangulated/stablecoin-proxy pairs 404s (`price-not-found`) because the cache key was never written. Pairs served from `prices_1m` directly still work. `flags.stale` does NOT fire (aggregator is running, it just cannot write).

**Most common cause:** root FS full, so Redis cannot write RDB snapshots and, with the default `stop-writes-on-bgsave-error yes`, refuses all writes (MISCONF). Aggregator log shows WARN `refresh failed` every tick with `err: "redis set vwap:...: MISCONF Redis is configured to save RDB snapshots, but it's currently unable to persist to disk..."`. Companion to [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full).

**Diagnose (1 min):**

```sh
# 1. Confirm Redis writes are blocked
ssh root@136.243.90.96 'redis-cli SET test:probe x'  # -> MISCONF Redis is configured to save RDB...

# 2. Confirm root FS at 100%
ssh root@136.243.90.96 'df -h /'

# 3. Check Redis log for rdbSaveRio errors
ssh root@136.243.90.96 'tail -20 /var/log/redis/redis-server.log'
# Expect: "Write error saving DB on disk(rdbSaveRio): No space left on device"
```

**Fix, step 1: free disk on `/`** (run ON r1; target < 80% used, 10 GB+ free), lowest-risk first:

```sh
# Vacuum systemd journal — keep recent 200 MB
journalctl --vacuum-size=200M

# Truncate rotated syslog archives
truncate -s 0 /var/log/syslog.1
# (also syslog.2.gz, .3.gz etc if present)

# Remove WASM-audit stderr captures (multi-GB per walk). The walk JSON and
# checkpoint JSONL are small and may not be committed yet, so they stay.
# -f is required: the kernel comm is truncated to 15 chars, so a bare
# `pgrep stellarindex-ops` never matches. Empty output = no walk running.
pgrep -af 'stellarindex-ops wasm-history'
rm -f /var/log/wasm-audit/*.stderr /var/log/wasm-history-*.stderr

# Postgres logs — only truncate if confirmed safe (not actively in use)
# Prefer `logrotate -f /etc/logrotate.d/postgresql-common` first
ls -la /var/log/postgresql/

# Confirm space is back
df -h /
```

**Fix, step 2: unblock Redis.** A successful BGSAVE clears the stop-writes flag:

```sh
ssh root@136.243.90.96 'redis-cli BGSAVE'      # -> Background saving started
ssh root@136.243.90.96 'redis-cli LASTSAVE'    # timestamp moves to ~now
ssh root@136.243.90.96 'redis-cli SET test:probe ok && redis-cli GET test:probe && redis-cli DEL test:probe'  # -> OK / ok / 1
```

**Fix, step 3: confirm aggregator recovered:**

```sh
ssh root@136.243.90.96 'journalctl -u stellarindex-aggregator --since "30 seconds ago" -o cat \
  | grep -c "refresh failed"'
# Expect: 0 (was 15+ per 30s pre-fix)

curl -sS "https://api.stellarindex.io/v1/price?asset=native&quote=<USDC-classic-asset_id>"
# Expect: 200 with observed_at within the last few minutes
```

Then address the underlying disk-full state per `postgres.md#stellarindex_timescale_disk_full`.

**Design note:** Redis is a cache only (all values reproducible from `trades`), so `stop-writes-on-bgsave-error no` (`CONFIG SET`) is an available trade-off (loses durability across restarts, aggregator keeps serving). The default `yes` is kept so an incident surfaces loudly.

**Prevention (in place):** root-FS alerts `stellarindex_node_root_disk_warning` (< 20% avail), `_full` (< 10%), `_filling_fast` (predict_linear) in `storage.yml`; logrotate/journald caps in `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml` (syslog `maxsize 100M`, 7 gzip rotations, journald `SystemMaxUse=500M`); wasm-audit walks write under `/var/log/wasm-audit/` ([`../wasm-audits/README.md`](../wasm-audits/README.md) §2).

**See also:** [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full); [`cache.md#stellarindex_redis_master_down`](cache.md#stellarindex_redis_master_down) (process exited); [`cache.md#stellarindex_redis_memory_saturated`](cache.md#stellarindex_redis_memory_saturated) (`stellarindex_redis_write_rejected_oom`, a different mechanism); [`api.md#stellarindex_ratelimit_fail_closed`](api.md#stellarindex_ratelimit_fail_closed); `internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md` (customer-facing post-mortem).

## stellarindex_aggregator_class_drop_spike

**Severity:** P3 (ticket). **Trigger:** `sum(rate(stellarindex_aggregator_dropped_trades_total{reason="class"}[10m]))` above 10x its `offset 1h` baseline, `for: 15m`. **MTTR:** 30 min.

**Impact:** the class filter drops trades at >10x baseline. Most often a new venue emits trades not yet in `external.Registry`, so they hit the fail-closed `IncludeInVWAP=false` fallback (`Class=Exchange`) and are excluded from VWAP. Ingest still records the rows; only aggregation is affected. New source labels also appear in `stellarindex_source_events_total`. The counter's `pair` label names which pair's VWAP the source was trying to feed.

**Diagnose (r1):**

```sh
ssh root@136.243.90.96

# 1) Which source is producing the unregistered traffic?
# :9464 = indexer (source_events_total); the
# dropped_trades{reason="class"} counter is on the aggregator at :9465.
curl -fs http://localhost:9464/metrics \
  | grep '^stellarindex_source_events_total{' | sort -t'"' -k2

# 2) Compare against what the registry knows.
grep -E '"[a-z][a-z0-9_-]+":\s*\{' \
  internal/sources/external/registry.go

# 3) Anything in the trades table from a never-before-seen source?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT source, COUNT(*) AS rows, MIN(timestamp) AS first_seen
   FROM trades
   WHERE timestamp > now() - interval '1 hour'
   GROUP BY source
   ORDER BY first_seen DESC
   LIMIT 20;"
```

Any source in `trades` absent from `internal/sources/external/registry.go` is the culprit.

**Fix:**
- Net-new venue being onboarded: add a one-line entry to `external.Registry` under the right `Class*` constant. A venue with a classifying ADR open and trade flow in Timescale is `ClassExchange + IncludeInVWAP=true`; a paid-tier aggregator/oracle stays `IncludeInVWAP=false`.
- Unintentional source name (typo, renamed connector, dev build in prod): find the rogue process and roll back. Its trades are valid; leave them (they show in `/v1/sources` once registered).
- Existing aggregator/oracle source spiked in volume: expected, those classes are meant to be dropped from VWAP. Document the cause (e.g. CoinGecko added pairs, Reflector upgraded) and silence for the interval; no code change.
- Verify: the `reason="class"` drop rate returns to baseline (after a registry update, same rate but `vwap_writes_total` also ticks up).

**RCA capture:** source-label list before/after, the PR/commit that added or renamed the source, confirmation the class assignment is intentional.

**False positives:** first hour after the rule lands (no `offset 1h` baseline yet; suppress on rollout); a dev binary scraped by prod Prometheus (check `stellarindex_source_enabled` for sources that should be off in this region).

**See also:** `internal/sources/external/registry.go` (single source of truth for class); [stellarindex_aggregator_outlier_storm](#stellarindex_aggregator_outlier_storm); per-source weighted VWAP is deferred, see [`aggregation-plan.md`](../../architecture/aggregation-plan.md).

## stellarindex_aggregator_fx_snap_fallback_dominant

**Severity:** P3 (ticket). **MTTR:** 15 min - 2 h. **Trigger** (`for: 30m`):

```promql
sum(rate(stellarindex_aggregator_fx_snap_fallback_total[15m]))
/
clamp_min(
  sum(rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])),
  1
) > 0.5
```

**Blind spot:** with `ok` pinned at zero the ratio can never reach 0.5, so a total leg drought is invisible here; that is [stellarindex_aggregator_triangulation_chains_dry](#stellarindex_aggregator_triangulation_chains_dry).

**Impact:** chained-fiat triangulation (e.g. XLM/EUR = XLM/USD x USD/EUR) is missing the bucket-end FX snap required by ADR-0018 §"Forex factor handling" (every region reads the same stored FX row for a closed bucket). On r1 all four default chains (XLM/EUR, XLM/GBP, ETH/GBP, BTC/GBP) use fiat/fiat FX legs, which have no VWAP cache key ever, so a snap miss is `outcome="missing_leg"` and the chain does NOT publish. Degraded-but-publishing (cached-VWAP fallback) holds only for crypto-leg chain configs. `/v1/price` chained-fiat responses stop updating.

**Where FX quotes come from:** the active feed is `massive`, a forex worker in the API binary (`internal/sources/external/forex`, key `MASSIVE_API_KEY`) writing hourly `rate_usd` rows to the `fx_quotes` hypertable. `FXQuoteAtOrBefore` (`internal/storage/timescale/trades.go`) reads `fx_quotes` first (7-day lookback), then `trades` filtered by `FXSources()`; the latter is always empty (no FX source writes `trades`; `exchangeratesapi`, disabled on r1, writes `oracle_updates`), so do not diagnose it. On `ErrNoFXQuote` the orchestrator tries the cached-VWAP fallback and increments `stellarindex_aggregator_fx_snap_fallback_total{leg=...}`.

**Diagnose:**

```sh
ssh root@136.243.90.96

# 1) Which legs are falling back? Cardinality is bounded, so the per-leg
#    counter directly names the affected pair(s).
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_fx_snap_fallback_total'

# 2) Is the massive feed fresh? (The external_fx_feed_stale alert
#    watches this — the worker runs in the API binary on :3000.)
curl -fs http://localhost:3000/metrics | grep external_fx_last_quote_unix

# 3) Are fresh fx_quotes rows landing?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT ticker, max(bucket) FROM fx_quotes WHERE ticker IN ('EUR','GBP') GROUP BY 1;"
```

| fx_quotes state | Cause | Action |
| --- | --- | --- |
| Fresh (< 2 h) | Snap-rule logic bug, FX healthy | File an issue; check recent commits to the triangulation snap path and `FXQuoteAtOrBefore`; check API and aggregator agree on `external.FXSources()` |
| Stale (2 h - 7 d) | massive worker lagging / poll failures | `stellarindex_external_fx_feed_stale` should also fire; follow [external-pollers.md](external-pollers.md#stellarindex_external_fx_feed_stale) |
| Older than 7 d | Beyond snap lookback, every snap misses | As above; chains are dry, expect `chains_dry` too |
| No rows | Worker never ran (fresh deploy / key missing) | Check `MASSIVE_API_KEY` in `/etc/default/stellarindex` (worker is in the API binary, NOT the indexer) |

**Fix:** feed stale/absent: sibling alerts `stellarindex_external_fx_feed_stale` / `_absent` (`configs/prometheus/rules.r1/external-pollers.yml`); usually an expired/missing `MASSIVE_API_KEY` or upstream Massive degradation; restart the API service after fixing the env. If chains stopped publishing (fiat/fiat legs), treat it as the `chains_dry` incident even before that alert's `for:` elapses. Verify: fallback rate near zero within 30 min of restoring the feed.

**RCA capture:** 1 h metric range of the spike and recovery; `external_fx_last_quote_unix` and API forex-worker logs; `fx_quotes` row activity per affected ticker; whether chains stopped publishing (`outcome="missing_leg"` rate).

**False positives:** first 30 min after a fresh deploy (absorbed by `for: 30m`); aggregator then API restart (clears in <= 5 min); bucket-end at exactly the latest FX row `ts` combined with > 1 s clock skew (query is `<=`; a region clock even 1 s ahead of the FX publish time moves the cutoff past the latest row so the next bucket's snap misses; skew is its own chrony/timesyncd alert).

**See also:** [stellarindex_aggregator_silent](#stellarindex_aggregator_silent) (zero writes across the board).

## stellarindex_aggregator_outlier_storm

**Severity:** P3 (ticket). **MTTR:** 30 min - several hours. **Trigger** (`for: 15m`): `max by (pair)(stellarindex_aggregator_venue_vwap{window="5m"}) / min by (pair)(...) - 1 > 0.01` with >= 2 venue series for that pair, i.e. venues disagree by > 1% on 5m VWAPs. An agreed market-wide move does not fire it; a ticket alongside a market move is about the venue that did not move.

**Impact:** one venue is stale, thin, mis-decoding amounts, or a stablecoin leg is skewed. The published price is protected (robust, time-locally filtered VWAP across venues): this is a data-quality / source-health signal, not a customer-facing price error. Cross-check the pair's `div:<pair>` Redis flag / API `flags.divergence_warning` for actual price impact; the live divergence signal is `stellarindex_divergence_max_abs_fraction`, which drives `stellarindex_price_divergence_{warning,critical}`.

**Filter background:** the filter is time-local (`internal/aggregate/outliers_local.go`): a print is dropped only when it disagrees with the whole window AND its own neighbourhood (own/adjacent 1-minute buckets, or nearest 5 prints each side on a thin series). Local references are anchored (trusted only when centred within ~4% of the window median or previous trusted reference; band capped at +-4%), so steps survive and lone wild prints / self-consistent spam bursts do not. `internal/aggregate/outliers.go` (whole-window form) is still used by `/v1/vwap` and `/v1/ohlc`. `aggregate.outlier_sigma_threshold` (default `4.0`) is the N in median + N*1.4826*MAD. Rule tests: `deploy/monitoring/rule-tests/aggregator_test.yml`.

**Diagnose:**

```sh
# 1) outlier_storm: which venue is the odd one out? One line per source.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_venue_vwap{' | grep 'window="5m"' | sort
# In PromQL, per pair:
#   stellarindex_aggregator_venue_vwap{pair="<pair>",window="5m"}

# 2) trim_fraction: how much of the window is the filter rejecting, and
#    how thin is the survivor set?
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_window_trades{' | grep 'window="24h"' | sort

# 2b) volume_trim_fraction: how much of the MONEY is the filter removing?
#    Read it beside window_trades for the same window: many prints but little
#    volume removed is dust; few prints and most of the volume is a large
#    print outside the band, or wash prints outvoting an honest block.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_window_base_volume{' | sort

# 3) Is the upstream-trade rate also elevated? (real volume -> real outliers)
psql -d stellarindex -c \
  "SELECT pair, source, COUNT(*) AS rows
   FROM trades
   WHERE timestamp > now() - interval '15 minutes'
   GROUP BY pair, source ORDER BY 3 DESC LIMIT 10;"

# 4) Per-tick drop counter, a diagnostic only (re-counts window residents every
#    tick: read it as "band-residents", not "new outliers/s"):
#   topk(5, rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m]))
```

| Trade rate elevated? | Multiple pairs? | Cause | Action |
| --- | --- | --- | --- |
| Yes | Many | One venue lagging a real market-wide move (stale feed, throttled poller) | Check that venue's poller freshness; wait it out |
| Yes | One pair | Pair-specific dislocation, possibly depeg | Check the pair's primary venue; consider pausing stablecoin proxy for that quote if a peg break |
| No | Many | Connector regression, weird amounts everywhere | Check `stellarindex_source_decode_errors_total`; likely a recent decoder change |
| No | One pair, one source | Single connector misbehaving (amount-decimal regression) | Disable that source via config; ticket against the connector |

**Spam-wave signature (SDEX token farm):** one issuer spamming self-trades across its own tokens. Drops concentrate on one configured pair, dropped rows show 25-37% price gaps between consecutive trades with dust-sized volumes, all tracing to one issuer account in `trades`. A single venue cannot disagree with itself, so `outlier_storm` stays silent and [stellarindex_aggregator_outlier_trim_fraction](#stellarindex_aggregator_outlier_trim_fraction) fires. Confirm with `window_trades{stage=...}` plus `topk` by `pair` on the drop counter, then the single-issuer pattern in `trades`. The filter is doing its job; treat as venue abuse and wait it out unless the survivor sample gets too thin (then see `min_usd_volume` gating; on an SDEX-only pair read survivor count and `min_usd_volume`). Residual gap: a wave still self-validates when it is (a) the majority of the whole 24h window (window median is then the spam level; `trim_fraction` reads low), (b) within the ~4% anchor tolerance of the honest level, or (c) random-walking in <= 4% bucket-to-bucket steps. A self-consistent 2-3x burst is dropped in full (`outliers_spam_test.go`).

**Fix:**
- Real market event: leave the filter alone; note timestamp and pairs in the on-call channel.
- Connector regression: identify the source via `stellarindex_source_events_total` x `stellarindex_source_decode_errors_total` ratio plus recent deploy diff. Set `[external.<venue>] enabled = false` in TOML to stop ingest, and add it to `aggregate.excluded_sources` to drop its stored trades from VWAP at read time (`enabled=false` alone leaves them in the window). Restart the aggregator; the `prices_1m` CAGG is not source-filtered.
- Filter mis-calibration (neither of the above, `trim_fraction` sustained > 1 h): raise `aggregate.outlier_sigma_threshold` 4.0 -> 5.0/6.0 while RCA continues. An agreed move being trimmed is a bug in the time-local filter, not calibration: capture pair + window and file it.
- Verify: venue spread (`max/min - 1` of `venue_vwap{window="5m"}`) < 1%, and `1 - window_trades{stage="outlier"} / {stage="class"}` < 0.2.

**RCA capture:** 1 h metric range; trade-table samples around the spike (source, pair, amounts, timestamp); for a connector regression the latest commit touching that source's `parse.go`/`decode.go`; for a market event external context.

**False positives:** a venue that stopped trading has its `venue_vwap` series DELETED on the next refresh, so a persisting series is a bug in `recordVenueVWAPs`. Stablecoin-leg skew: a USDT-quoted vs USD-quoted venue diverge by the USDT/USD basis; > 1% sustained is a real depeg, escalate to the pricing owner. A new pair's first tick (< 3 valid prices): `aggregate.FilterOutliersLocal` is a no-op and `trim_fraction` needs >= 20 trades; both stay silent.

**See also:** [stellarindex_aggregator_silent](#stellarindex_aggregator_silent) (often co-fires when the filter removes every row); [`aggregation-plan.md`](../../architecture/aggregation-plan.md#open-and-deferred) "Open and deferred". Any filter algorithm change must update this runbook, the `outliers_spam_test.go` fixture numbers and the promtool `trim_fraction` case together.

## stellarindex_aggregator_outlier_trim_fraction

**Severity:** P3 (ticket). **Trigger** (`for: 30m`): `window="24h"` and `1 - window_trades{stage="outlier"} / window_trades{stage="class"} > 0.2` with `window_trades{stage="class"} >= 20`.

**Meaning:** the time-local filter is rejecting > 20% of one pair's 24h window: a spam / wash / dust wave inside the window, typically single-venue (what [stellarindex_aggregator_outlier_storm](#stellarindex_aggregator_outlier_storm) cannot see; proven fireable on the token-farm fixture, 40% trim share). The published price stays protected; the harm is a source wholesale-rejected (completeness) or a thin surviving sample.

**Diagnose and fix:** step 2 (`window_trades` per stage), step 3 (trade rate by pair/source) and step 4 (`topk` on the drop counter), the spam-wave signature and the Fix list are in the shared section: see [stellarindex_aggregator_outlier_storm](#stellarindex_aggregator_outlier_storm). Silent below 20 trades. Verify: the ratio returns under 0.2. If sustained > 1 h with no spam or connector cause, raise `aggregate.outlier_sigma_threshold` as described there.

## stellarindex_aggregator_outlier_volume_trim_fraction

**Severity:** P3 (ticket). **Trigger** (`for: 15m`): on any window, `1 - window_base_volume{stage="outlier"} / window_base_volume{stage="class"} > 0.2` with class `window_base_volume > 0`.

**Meaning:** the filter removed > 20% of one pair's traded base volume in one window. A value of exactly 1 with `window_trades{stage="class"} > 0` means the window is WITHHELD (contested, no price published for it): the prints the count majority keeps carry less volume than those it drops (wash-over-block shape). Visible where the 24h count rule and the >= 2-venue storm rule are not (withheld 5m/1h window, single-venue wash burst).

**Diagnose:** read `window_base_volume{stage}` (step 2b) beside `window_trades` for the same window: many prints but little volume removed = dust; few prints and most volume removed = a large print outside the band, or wash prints outvoting an honest block. Then trade rate, drop-counter `topk` and the spam signature in [stellarindex_aggregator_outlier_storm](#stellarindex_aggregator_outlier_storm). Fix steps and verification are shared there; withheld windows clear once the contested prints leave the window or the wash source is excluded (`aggregate.excluded_sources`).

## stellarindex_aggregator_silent

**Severity:** P1 (page). **Trigger** (`for: 5m`): `sum(rate(stellarindex_aggregator_vwap_writes_total[5m])) == 0 OR absent_over_time(stellarindex_aggregator_vwap_writes_total[10m]) == 1`. **MTTR:** 5-20 min.

**Impact:** zero VWAP cache writes for 5+ min. `/v1/price` serves progressively staler cached VWAPs (`flags.stale` flips, `observed_at` ages) and rewritten/triangulated pairs 404 as key TTLs lapse (`cachekeys.VWAP`). `/v1/vwap` is unaffected: it always scans raw trades on-query (`internal/api/v1/vwap.go:153-157`).

**Diagnose:**

```sh
ssh root@136.243.90.96

# 1) Is the binary alive at all?
systemctl status stellarindex-aggregator
journalctl -u stellarindex-aggregator -n 50 --no-pager

# 2) What does the orchestrator say about its last tick?
# Use :9465 for the aggregator; :9464 is the INDEXER's port. The aggregator
# auto-shifts its metrics listener 9464 -> 9465 when `obs.metrics_listen` is
# left at its default (cmd/stellarindex-aggregator/main.go:2114-2116); to pin
# a different port set `obs.metrics_listen` in the aggregator's TOML.
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_aggregator_(ticks_total|empty_windows|dropped_trades|vwap_writes)'

# 3) Is Redis reachable and accepting writes? (single local
# redis-server on localhost — no auth, no replica)
redis-cli PING
redis-cli SET _aggregator_probe "$(date -Iseconds)" EX 60

# 4) Is Timescale serving trades for the configured pair set?
runuser -u postgres -- psql -d stellarindex -c "SELECT pair, COUNT(*) FROM trades WHERE timestamp > now() - interval '5 minutes' GROUP BY pair ORDER BY 2 DESC LIMIT 10;"
```

| Metrics show | Meaning | Next |
| --- | --- | --- |
| `ticks_total{outcome="ok"}` advancing, `vwap_writes_total` flat | Every (pair, window) lands in the empty-window branch | `empty_windows_total` should match pairs x windows rate; then check Timescale trade volume |
| only `ticks_total{outcome="error"}` advancing | Refresh-loop failures | `journalctl` for `refresh failed`; Timescale or Redis side |
| `ticks_total` flat | Tick loop not running (alive but stuck) | `pprof` goroutines, or restart |

**Fix:**
- Redis proximate cause (PING fails / writes error): fix the local redis-server; there is NO standby (single Debian-packaged redis-server on localhost, no sentinel/replica/auth). Restart the unit; if writes fail with `MISCONF` (BGSAVE blocked by full root FS) free disk per [stellarindex_aggregator_cache_write_errors](#stellarindex_aggregator_cache_write_errors). The aggregator retries next tick, no restart needed.
- Tick loop wedged: `systemctl restart stellarindex-aggregator`; first tick is immediate, so writes resume within `interval_seconds` (default 30 s).
- Empty windows (Timescale has trades but every configured pair returns zero rows): check whether `enable_stablecoin_fiat_proxy` is needed; a fiat-quote pair (`XLM/fiat:USD`) without expansion only matches direct FX-feed trades, sparse if FX connectors are off.
- Verify: `vwap_writes_total` advances within one tick interval; alert clears within `for: 5m`.

**RCA capture:** `journalctl -u stellarindex-aggregator --since='15 minutes ago'`; `/metrics` snapshots before/after (`ticks_total`, `dropped_trades_total`); concurrent Timescale/Redis incidents; current `cfg.Aggregate.*` plus configured `Pairs`/`Windows` (config drift if expansion is off but wanted).

**False positives:** aggregator not deployed in this environment (dev/staging; the `absent_over_time` branch fires with no series; silence via AlertManager routing; the rule has no `up{job=...}` precondition and the r1 job is `stellarindex-aggregator`). First 60 s after a fresh boot should never trigger (immediate first tick); if it does suspect config or storage bring-up.

**See also:** ADR-0007 (why VWAP is pre-computed); [stellarindex_aggregator_outlier_storm](#stellarindex_aggregator_outlier_storm) (a market event should NOT drive writes to zero; the time-local filter does not trim agreed shifts).

## stellarindex_aggregator_triangulation_chains_dry

**Severity:** P3 (ticket). **MTTR:** 30 min - 4 h. **Trigger** (`for: 30m`): `sum(rate(stellarindex_aggregator_triangulations_total{outcome="missing_leg"}[15m])) > 0 and sum(rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])) == 0`.

**Impact:** the composite-rate corroboration factor (`triangulation_agreement`) contributes nothing; thin pairs (XLM/GBP, XLM/EUR, ETH/GBP) lose one independent check on single-source prints (`triangulation_checked=false` on the wire). Served prices are unaffected; direct rates still publish. No customer-visible error.

**Mechanism:** four default chains (XLM/EUR, XLM/GBP, ETH/GBP, BTC/GBP) = deep USD pair x fiat cross. The fiat/fiat leg does not resolve through the VWAP cache (no producer); it resolves via `Store.FXQuoteAtOrBefore` over `fx_quotes` (daily buckets, source `massive`, written by the forex worker in the API binary) with a 7-day lookback (`fxQuotesSnapLookback`). No row in the lookback means snap miss, cached-VWAP miss, `missing_leg`; the chain silently does not publish. [stellarindex_aggregator_fx_snap_fallback_dominant](#stellarindex_aggregator_fx_snap_fallback_dominant) cannot see this: its numerator (~0.4/s for four chains) sits under its 0.5 floor and its `clamp_min(ok_rate, 1)` denominator bounds the ratio with `ok` at zero. It covers partial degradation; this covers total drought.

**Diagnose:**

```sh
# 1) Confirm the shape: missing_leg counting, ok flat at zero.
#    (All five outcomes are pre-seeded at zero, so a genuine 0 here is
#    a real zero, not a scrape gap.) Aggregator metric: :9464 is the
#    indexer's port; the aggregator auto-shifts to :9465 on a single-host
#    deploy (see aggregator_silent).
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_triangulations_total'

# 2) Is the fiat-FX feed alive at all? This gauge only advances on a
#    committed non-empty fx_quotes batch. The forex worker runs inside
#    the API binary, which serves /metrics on its public listener
#    (config's metrics_listen is ignored by the API binary).
curl -fs http://localhost:3000/metrics \
  | grep '^stellarindex_external_fx_last_quote_unix'

# 3) Is there an fx_quotes row inside the 7-day snap lookback for the
#    crosses the chains need?
psql -d stellarindex -c \
  "SELECT ticker, source, max(bucket_day) AS latest, now()::date - max(bucket_day) AS days_old
     FROM fx_quotes
    WHERE ticker IN ('EUR','GBP')
    GROUP BY ticker, source
    ORDER BY latest DESC;"
```

| `fx_quotes` newest row | Cause | Fix |
| --- | --- | --- |
| Older than 7 days, or table empty | Forex worker dead / not running in API binary / never started | Fix 1 |
| Fresh, but only tickers the chains do not use | Chain references a cross with no feed | Fix 2 |
| Fresh for EUR + GBP | Not FX: the chain's crypto leg is missing from the VWAP cache | Fix 3 |

**Fix:**
1. Dry FX feed (common): the `massive` forex worker runs inside the API binary. Check it is up and its last poll succeeded (`stellarindex_external_fx_last_quote_unix{source="massive"}`), then the upstream credential/quota. Chains recover on the next aggregator tick after the first committed batch; no aggregator restart.
2. Chain against a cross with no feed: chains come from the ansible template layer, deliberately not `config.Default()`. Add the missing cross to the forex worker's ticker list or drop the chain from the deployment's `aggregate.triangulations` block.
3. Crypto leg missing: `missing_leg` is also returned when the non-fiat leg's VWAP key is absent from Redis. Confirm the deep USD pair (e.g. XLM/USD) publishes; if not, this is downstream of [stellarindex_aggregator_silent](#stellarindex_aggregator_silent) or the pair's own freshness alerts.
4. Verify: `outcome="ok"` increments within one tick of the legs resolving; alert clears after its 30 min `for:`.

**False positives:** fresh deployment before the forex worker's first batch (absorbed by `for:`); no chains configured (alert cannot fire, a deployment choice); aggregator fully down (both series absent, `and` yields nothing, [stellarindex_aggregator_silent](#stellarindex_aggregator_silent) owns it).

**See also:** `internal/aggregate/orchestrator/triangulate.go` (chain evaluation, outcome labels); `internal/storage/timescale/fx_quotes.go`; `docs/reference/metrics/README.md` § `stellarindex_aggregator_triangulations_total`.

## stellarindex_asset_character_rollup_failing

**Severity:** P3 (informational). **MTTR:** 5-15 min to fix the cause; recovery lands on the next 6h sweep. **Trigger** (`for: 5m`): `sum(increase(stellarindex_asset_character_rollup_sweeps_total{outcome="refresh_error"}[13h])) > 0 unless sum(increase(stellarindex_asset_character_rollup_sweeps_total{outcome="ok"}[13h])) > 0`. The worker sweeps every 6h, so the 13h window means two consecutive sweeps failed.

**Impact:** the `volume_character` label and signals on `/v1/assets` and `/v1/assets/{id}` hold their last-good value (stale, not blank). No pricing impact.

**Diagnose:**

```sh
curl -s localhost:9464/metrics | grep stellarindex_asset_character_rollup_sweeps_total
journalctl -u stellarindex-aggregator --since -12h | grep -i "asset-character rollup"
sudo -u postgres psql stellarindex -c '\d asset_volume_character'
sudo -u postgres psql stellarindex -c \
  'SELECT count(*), max(computed_at) FROM asset_volume_character;'
```

**Fix:** missing table means migration 0149 did not apply; run the migrator (deploy.yml auto-applies). Postgres down / lock contention / statement timeout: follow the storage runbook; the worker retries every 6h, there is no manual catch-up, and a restart re-runs the first sweep after a 30 min startup delay.

**See also:** [`stellarindex_asset_character_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_asset_character_rollup_sweeps_total); worker `internal/aggregate/assetcharacterrollup/worker.go`; table `migrations/0149_create_asset_volume_character_rollup.up.sql`; [alerts-catalog.md](../alerts-catalog.md).

## stellarindex_asset_volume_rollup_failing

**Severity:** P3 (informational). **MTTR:** 5-15 min (almost always Postgres reachability, shared with louder alerts). **Trigger** (`for: 30m`): `sum(rate(stellarindex_asset_volume_rollup_sweeps_total{outcome="refresh_error"}[15m])) > 0`; the worker sweeps every ~2 min, so single blips do not fire it.

**Impact:** `/v1/assets` `volume_24h_usd` stops advancing (last-good value, not zero) and the volume-desc sort order freezes. No customer pricing impact.

**Symptoms:** `stellarindex_asset_volume_rollup_sweeps_total{outcome="refresh_error"}` rising with `outcome="ok"` flat; `journalctl -u stellarindex-aggregator | grep "asset-volume rollup refresh failed"` shows the wrapped Postgres error about every 2 min; explorer assets list shows frozen volume column.

**Diagnose:**

```sh
# Is it failing, and how often?
curl -s localhost:9464/metrics | grep stellarindex_asset_volume_rollup_sweeps_total

# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-aggregator --since -30min | grep -i "asset-volume rollup"

# refresh_error is almost always Postgres. Check the table exists
# (migration 0087):
sudo -u postgres psql stellarindex -c '\d asset_volume_24h'

# Spot-check the rollup has fresh rows (computed_at should be within a
# few minutes):
sudo -u postgres psql stellarindex -c \
  'SELECT count(*), max(computed_at) FROM asset_volume_24h;'
```

**Fix:** missing table means migration 0087 did not apply; run the migrator (deploy.yml auto-applies; manual: `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up`). Postgres down / lock contention: follow the storage runbook; the alert clears on the next successful sweep (worker retries forever; upsert-then-prune is idempotent). No catch-up step: the next sweep recomputes the full trailing-24h volume.

**RCA:** the worker runs one trailing-24h base-OR-quote SUM over the `prices_1m` CAGG (all pairs) plus upsert + prune every 2 min; it is the heavier of the two 24h rollups. Failure with healthy `/v1/price` traffic almost always means lost Postgres reachability or served-tier lock/IO pressure; check what else fired. If `stellarindex_asset_volume_rollup_sweep_duration_seconds` climbs toward the cadence, the scan is getting heavier and the cadence may need widening.

**See also:** [`stellarindex_asset_volume_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_asset_volume_rollup_sweeps_total), [`stellarindex_asset_volume_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_asset_volume_rollup_sweep_duration_seconds); worker `internal/aggregate/assetvolrollup/worker.go` (wired in `cmd/stellarindex-aggregator/main.go`); SQL `internal/storage/timescale/coins.go`; table `migrations/0087_create_asset_volume_24h_rollup.up.sql`; [alerts-catalog.md](../alerts-catalog.md).

## stellarindex_change_summary_stale

**Severity:** P3 (ticket). **MTTR:** 15 min. **Trigger** (`for: 15m`): `(time() - stellarindex_change_summary_last_success_unix{job="stellarindex-aggregator"}) > 1800 and stellarindex_change_summary_last_success_unix{job="stellarindex-aggregator"} > 0`: no pass of the change-summary worker upserted any entity for 30 min. `stellarindex_change_summary_passes_total{outcome="failed"}` is typically rising.

**Impact:** explorer delta strips (h1/h24/d7/d30, ATH/ATL, streak) serve frozen rows.

**Diagnose:**

```sh
systemctl is-active stellarindex-aggregator
journalctl -u stellarindex-aggregator --since "-30min" | grep 'change-summary pass had failures'
```

The warn carries `first_err`. `no closed observations in window` for every entity means `prices_1m` is not advancing (check price CAGG refresh and ingest freshness). A Postgres error means reachability, lock contention, or a missing `change_summary_5m` migration.

**Fix:** fix the cause; the next 5-minute pass upserts every entity. Verify `stellarindex_change_summary_last_success_unix` advances within 5 min and the alert clears.

**False positive:** `partial` passes are normal (a pair with no recent trades fails each pass) and do not trip it; only passes with zero upserts do.

**See also:** `internal/aggregate/changesummary/rollup.go`; [stellarindex_aggregator_silent](#stellarindex_aggregator_silent) (aggregator process itself down).

## stellarindex_decimals_guard_sweep_stale

**Severity:** P3 (ticket). **Trigger** (`for: 15m`): `(time() - stellarindex_decimals_guard_sweep_last_success_unix{job="stellarindex-aggregator"}) > 2700 and stellarindex_decimals_guard_sweep_last_success_unix{job="stellarindex-aggregator"} > 0`.

**Meaning:** the aggregator's decimals guard (`internal/decimalsguard`, sweep interval 15 m) has not completed a `Sweep` pass in over 45 min (counted from the last pass, or from aggregator start if none): it never armed (ClickHouse lake dial still retrying or startup Backfill running) or wedged. The offender counters (`stellarindex_dex_trade_nonstandard_decimals_total`, `stellarindex_nonstandard_decimals_lockstep_mismatch_total`) are untrustworthy meanwhile. A gauge at 0 means the guard is deliberately disabled (`decimals-guard: disabled` logged at startup), which the `> 0` arm excludes.

**First diagnosis:**

```sh
journalctl -u stellarindex-aggregator | grep decimals-guard
```

Look for `decimals-guard: ClickHouse decimals resolver unavailable` / `decimals-guard started` / `decimals-guard exited with error`, and check ClickHouse reachability. Restart the aggregator if wedged. Rest: [dex.md#dex-nonstandard-decimals](dex.md#dex-nonstandard-decimals).

## stellarindex_nonstandard_decimals_correction_failing

**Severity:** P3 ticket (the real action item). **Trigger** (`for: 5m`), any of: increase over 15 m (or a newly appeared non-zero series) of `stellarindex_nonstandard_decimals_cache_refresh_failures_total` (API `NonstandardDecimalsCache` refresh against Postgres failing), `stellarindex_price_serve_declined_nonstandard_decimals_total` (a serving path declines instead of normalizing; should be permanently zero, a regression signal), or `stellarindex_nonstandard_decimals_lockstep_mismatch_total{site,asset}` (the lake `decimals()` and the `nonstandard_decimals_assets` projection disagree for `asset`; only this arm names the token). While it fires, newly detected non-7-decimal tokens may serve raw, skewed ratios.

**First diagnosis:**

```sh
curl -s http://localhost:9465/metrics | grep -E 'nonstandard_decimals_cache_refresh_failures_total|price_serve_declined_nonstandard_decimals_total'
journalctl -u stellarindex-aggregator | grep decimals-guard
```

For a lockstep hit read the ERROR line `decimals-guard: nonstandard_decimals_assets row DISAGREES with the lake` for `persisted_decimals` vs `lake_decimals`. Mitigation, hand-seeding and the lockstep `site` meanings: [dex.md#dex-nonstandard-decimals](dex.md#dex-nonstandard-decimals).

## stellarindex_customer_webhook_fanout_failing

Fan-out failure means no delivery row was written, so nothing retries. Delivery attempts that do exist retry on a 15-attempt budget whose last retry lands ~4–8 h after the event (jittered backoff, 30 s doubling to a 1 h cap); see [stellarindex_customer_webhook_delivery_failing](api.md#stellarindex_customer_webhook_delivery_failing).

**Severity:** P3 (ticket). **MTTR:** minutes once Postgres writes are healthy; the re-emit is the slow part. **Trigger** (`for: 5m`): `sum by (event_type, reason) (increase(stellarindex_customer_webhook_fanout_failures_total[1h])) > 0`. One occurrence is worth a ticket.

**Impact:** a subscribed customer did not get a delivery row written; nothing retries, so the event is permanently lost to them until an operator re-emits. The counter increments in `customerwebhook.Fanout.Publish`, the producer side, which runs in the aggregator (freeze and divergence hot paths) and `stellarindex-ops emit-incident`. Distinct from [customer-webhook-delivery-failing](api.md#stellarindex_customer_webhook_delivery_failing), which watches delivery attempts after a `webhook_deliveries` row exists (15-attempt retry budget, ~4-8 h, then `exhausted`). Zero delivery failures alongside this alert is expected.

| `reason` | Meaning | Blast radius |
| --- | --- | --- |
| `enqueue` | One subscriber's `EnqueueDelivery` INSERT failed | That subscriber only; counter counts lost deliveries |
| `list_subscribers` | `ListWebhooksSubscribedTo` errored | Every subscriber of that event type |
| `invalid_payload` | Producer passed non-JSON to `Publish` | Every subscriber; code bug, not an outage |

**Diagnose:**
1. Event type and reason from the alert labels (`{{ $labels.event_type }}` / `{{ $labels.reason }}`):

```promql
sum by (event_type, reason) (
  increase(stellarindex_customer_webhook_fanout_failures_total[1h])
)
```

2. Producer log: each increment pairs with a WARN in the fan-out and an ERROR at the call site naming the lost event (`asset`/`quote` for `anomaly.freeze`, `pair` for `divergence.firing`, plus `subscribers`/`enqueued`/`failed`):

```sh
journalctl -u stellarindex-aggregator --since '-2h' \
  | grep -E 'customerwebhook.fanout|fan-out lost'
```

3. `enqueue` and `list_subscribers` are `webhook_deliveries` / `customer_webhooks` access failures: check platform Postgres (connection saturation, disk, statement timeout, app-role grants per `migrations/README.md` rule 7):

```sh
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT count(*) FROM webhook_deliveries WHERE created_at > now() - interval '\''1 hour'\'';
"'
```

**Fix:**
1. Fix the store first.
2. Identify losses from the durable source of truth, then cross-reference the window against `webhook_deliveries` (rows that should exist for a subscriber and do not are the losses):

| `event_type` | Source of truth |
| --- | --- |
| `anomaly.freeze` | `freeze_events` |
| `divergence.firing` | `divergence_runs` (+ Redis cached result) |
| `incident.sev1` / `incident.resolved` | incident markdown under `deploy/comms/` |
| `price.alert` | `price_alerts` (emitted by `internal/pricealerts/worker.go`, which enqueues directly, not via `Fanout`; its series are pre-seeded and will not move until it is migrated onto `Fanout`) |

3. Re-emit: incidents via `stellarindex-ops emit-incident -slug <slug> -event <sev1|resolved> -write` (exits non-zero if the fan-out loses anything; zero exit confirms). Freeze/divergence have no re-emit command: contact the customer directly if they depend on them. The `emit-incident` process is short-lived and never scraped; it returns the error to the shell.
4. `invalid_payload`: a code bug; find the call site from the log line and fix the marshalling.

**Do NOT:** treat this as covered by the delivery alerts (they watch a table this failure never wrote to); make the fan-out blocking (it would stall the price pipeline on a webhook-store blip; the error return exists to be logged and counted).

**See also:** [customer-webhook-delivery-failing](api.md#stellarindex_customer_webhook_delivery_failing); [admin-audit-write-failing](api.md#stellarindex_admin_audit_write_failing).

## stellarindex_protocol_events_rollup_failing

**Severity:** P3 (informational). **MTTR:** 5-15 min (almost always Postgres reachability, shared with louder alerts). **Trigger** (`for: 30m`): `sum(rate(stellarindex_protocol_events_rollup_sweeps_total{outcome="refresh_error"}[15m])) > 0`; sweeps run every ~2 min, so single blips do not fire it.

**Impact:** `events_24h` on `/v1/protocols` and `/v1/protocols/{name}` stops advancing (last-good value, not zero). No customer pricing impact.

**Symptoms:** `stellarindex_protocol_events_rollup_sweeps_total{outcome="refresh_error"}` rising with `outcome="ok"` flat; `journalctl -u stellarindex-aggregator | grep "protocol-events rollup refresh failed"` shows the wrapped Postgres error every ~2 min; protocol pages show a frozen `events_24h`.

**Diagnose:**

```sh
# Is it failing, and how often?
curl -s localhost:9464/metrics | grep stellarindex_protocol_events_rollup_sweeps_total

# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-aggregator --since -30min | grep -i "protocol-events rollup"

# refresh_error is almost always Postgres. Check the table exists
# (migration 0086):
sudo -u postgres psql stellarindex -c '\d protocol_events_24h'

# And that the census still runs by hand (this is the query the worker
# folds into the rollup):
sudo -u postgres psql stellarindex -c 'SELECT source, events_24h FROM protocol_events_24h ORDER BY events_24h DESC;'
```

**Fix:** missing table means migration 0086 did not apply; run the migrator (deploy.yml auto-applies; manual: `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up`). Postgres down / lock contention: follow the storage runbook; the alert clears on the next successful sweep (worker retries forever; upsert-then-prune is idempotent). No catch-up step: the next sweep recomputes the full trailing-24h census.

**RCA:** one census transaction (UNION ALL count over the served protocol hypertables) plus upsert + prune every 2 min. Failure with healthy `/v1/price` traffic almost always means lost Postgres reachability or served-tier lock pressure; check what else fired.

**See also:** [`stellarindex_protocol_events_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_protocol_events_rollup_sweeps_total), [`stellarindex_protocol_events_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_protocol_events_rollup_sweep_duration_seconds); worker `internal/aggregate/protoeventsrollup/worker.go` (wired in `cmd/stellarindex-aggregator/main.go`); SQL `internal/storage/timescale/protocol_stats.go`; table `migrations/0086_create_protocol_events_24h_rollup.up.sql`; [alerts-catalog.md](../alerts-catalog.md).

## Related

- [dex-nonstandard-decimals](../runbooks/dex.md#dex-nonstandard-decimals) (`docs/operations/runbooks/dex.md#dex-nonstandard-decimals`): full decimals-guard procedure.
- [redis-write-blocked-disk-full](cache.md#stellarindex_redis_writes_blocked): shared with the storage family.
