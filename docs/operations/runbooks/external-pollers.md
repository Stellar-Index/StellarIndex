---
title: Runbook — external pollers, CEX streams, FX feed
last_verified: 2026-10-06
status: draft
severity: P2 for `stellarindex_external_poller_stale`, `stellarindex_external_fx_feed_stale`, `stellarindex_cex_stream_subscription_rejected` and `stellarindex_cex_stream_entry_skips`; P3 for the rest
---

# Runbook — external pollers, CEX streams, FX feed

Rules: `configs/prometheus/rules.r1/external-pollers.yml` (loaded on r1) and `deploy/monitoring/rules/external-pollers.yml` (multi-host template). Exprs, `for:` and severities are identical in both trees; only the `stellarindex_external_poller_stale_ecb` description text differs (deploy tree adds the ECB URL and an "informational because fallback FX" note, though its `severity` is `ticket`). Group `stellarindex.external_pollers`, interval 60s, labels `team: stellarindex`, `component: external-poller`.

## At a glance

- [`stellarindex_external_poller_stale`](#stellarindex_external_poller_stale)
- [`stellarindex_external_poller_stale_ecb`](#stellarindex_external_poller_stale_ecb)
- [`stellarindex_external_poller_stale_tiingo`](#stellarindex_external_poller_stale_tiingo)
- [`stellarindex_external_poller_error_rate_high`](#stellarindex_external_poller_error_rate_high)
- [`stellarindex_chainlink_feed_stale`](#stellarindex_chainlink_feed_stale)
- [`stellarindex_external_fx_feed_stale`](#stellarindex_external_fx_feed_stale)
- [`stellarindex_external_fx_feed_absent`](#stellarindex_external_fx_feed_absent)
- [`stellarindex_external_fx_rate_rejections`](#stellarindex_external_fx_rate_rejections)
- [`stellarindex_fx_fixings_refresh_stale`](#stellarindex_fx_fixings_refresh_stale)
- [`stellarindex_fx_fixings_series_stale`](#stellarindex_fx_fixings_series_stale)
- [`stellarindex_fx_fixings_quote_disagreement`](#stellarindex_fx_fixings_quote_disagreement)
- [`stellarindex_cex_stream_subscription_rejected`](#stellarindex_cex_stream_subscription_rejected)
- [`stellarindex_cex_stream_entry_skips`](#stellarindex_cex_stream_entry_skips)
- [`stellarindex_cex_stream_stalled`](#stellarindex_cex_stream_stalled)
- [`stellarindex_external_dust_dropped_high`](#stellarindex_external_dust_dropped_high)

## Shared context

- `external.Connector` pollers (CoinGecko, CoinMarketCap, CryptoCompare, ECB, ExchangeRatesAPI, Tiingo, Chainlink) and the CEX WebSocket streamers (Binance, Coinbase, Kraken, Bitstamp) run in `stellarindex-indexer`. Metrics: indexer `:9464` (loopback; `:9100` is node_exporter; the aggregator shifts to `:9465`). Query from the host.
- `massive` (fiat FX, `internal/sources/external/forex` worker) runs in `stellarindex-api`, not the poller framework: no `stellarindex_external_poller_*` series, so poller alerts cannot see it. Its alerts are the `fx_*` ones below.
- Impact of a lost poller: the venue drops out of its pairs' consensus; `/v1/price` keeps serving from the remaining sources (a thinner consensus can surface as `flags.single_source` and elevated `flags.divergence_warning`). Whether VWAP moves depends on the source's `Class` / `IncludeInVWAP` row in `internal/sources/external/registry.go` (see [aggregation-plan](../../architecture/aggregation-plan.md)); oracle- and lending-class sources never contribute to VWAP (`internal/sources/external/registry.go`). CoinGecko has two independent paths (the ingest poller and `divergence.CoinGeckoReference`), so a stale poller does not blind the cross-reference layer.
- Logs: `ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "1 hour ago" --no-pager | grep -E "poller error|poller stopping|produced no rows|no applicable pairs" | grep <source>'`
- Startup line showing the auth tier: `journalctl -u stellarindex-indexer --no-pager | grep -F 'external poller enabled' | grep -F 'source=coingecko' | tail -1` gives `… poll_interval=5m0s auth_mode=anonymous|demo|pro`.
- Verify a fix: `ssh root@136.243.90.96 "curl -s http://localhost:9464/metrics | grep -E 'stellarindex_external_poller_(polls|last_success).*<source>'"` shows `stellarindex_external_poller_polls_total{source="<source>",outcome="success"}` incrementing and `stellarindex_external_poller_last_success_unix{source="<source>"}` recent.
- Env-wired keys live in `/etc/default/stellarindex` (the `EnvironmentFile=` of the units; templated from `configs/ansible/roles/archival-node/templates/stellarindex.env.j2`): `COINGECKO_API_KEY` (Pro, header `x-cg-pro-api-key`, wins), `COINGECKO_DEMO_API_KEY` (header `x-cg-demo-api-key`), `MASSIVE_API_KEY`, `OPENEXCHANGERATES_APP_ID`.
- Codify a key change, never hand-edit (the next playbook run overwrites it):

  ```sh
  # on the workstation, from configs/ansible/
  ansible-vault edit inventory/r1.secrets.yml     # set vault_coingecko_demo_api_key
  ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
    --tags stellarindex --check --diff            # always --check --diff first
  ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
    --tags stellarindex
  ```

  The role's handler restarts the unit; by hand: `ssh root@136.243.90.96 'systemctl daemon-reload && systemctl restart stellarindex-indexer'` (API-side keys: `systemctl restart stellarindex-api`).

## stellarindex_external_poller_stale

Trips: `(time() - stellarindex_external_poller_last_success_unix{source!~"ecb|tiingo"}) > 1800`, `for: 5m`, `severity: ticket` (P2). A poller has had no successful `PollOnce` for 30 min: venue rejecting us (auth / rate limit), venue down, or network path broken. ECB and Tiingo are split out (next two sections); the budget is not 30 min for every source.

Diagnose: the label `source` names the poller; run the shared log grep. Decode the latest error:

| Error contains | Cause | Action |
|---|---|---|
| `http 429` | rate-limited | higher-tier API key (CoinGecko: below) |
| `http 401` / `http 403` | auth failure | rotate / re-issue the key |
| `http 5..` | venue outage | wait; check the venue status page |
| `http: timeout` | network slowness | check r1 public egress |
| `dial tcp: ... no route` | DNS / allowlist / firewall | check r1 networking, ufw, DNS |
| `decode` / `unmarshal` | venue changed shape | bug: patch the decoder, file a PR |
| `produced no rows` | 200 with nothing usable (`outcome="empty"`): renamed slug/symbol or delisted pair | fix the id in the source's seed/config |
| `no applicable pairs` (`outcome="idle"`) | no configured pair maps to this source | fix the pair list in the indexer TOML or remove the source |

Fix:
- CoinGecko `http 429` every minute (post-2024 unauthenticated-tier tightening): register a free demo key at coingecko.com/en/developers/dashboard, set it via the codified flow above, then confirm `auth_mode=demo` (or `pro`) on the next startup. See `stellarindex_external_poller_error_rate_high` for the tier/budget matrix; check it BEFORE rotating keys.
- Paid key expired (`http 401/403` with a key set; common for CMC / CryptoCompare at annual renewal): renew in the venue dashboard, rerun the vault + playbook cycle.
- Venue outage (`http 5..` / `connection refused`): wait for the venue; record in `docs/operations/incidents/` if > 1 h.
- Verify per the shared section.

## stellarindex_external_poller_stale_ecb

Trips: `(time() - stellarindex_external_poller_last_success_unix{source="ecb"}) > 43200` (12 h), `for: 10m`, `severity: ticket` (P3). ECB publishes once per EU business day and the poller interval is 6 h (`internal/sources/external/ecb/poller.go::DefaultPollInterval`), so 12 h is two missed cycles; the 30 min rule would have fired after every successful poll. Do not read this as "30 min stale".

Causes: rates.xml.zip endpoint down (rare; the deploy-tree description cites `https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml`), the host lost outbound HTTPS to ECB, or the poller goroutine deadlocked. Diagnose and fix as `stellarindex_external_poller_stale` with `<source>` = `ecb`. Note the forex worker's own ECB standby (see `stellarindex_external_fx_feed_stale`) is a different code path.

## stellarindex_external_poller_stale_tiingo

Trips: `(time() - stellarindex_external_poller_last_success_unix{source="tiingo"}) > 7200` (2 h), `for: 10m`, `severity: ticket` (P3). Tiingo's poll interval is 1 h; one whole cycle was skipped. Causes: Tiingo API down or throttling (4xx/5xx), API token expired, or host lost outbound HTTPS. Diagnose and fix as `stellarindex_external_poller_stale` with `<source>` = `tiingo`.

## stellarindex_external_poller_error_rate_high

Trips, `for: 15m`, `severity: informational` (no ticket; escalation is `stellarindex_external_poller_stale`):

```promql
(
  rate(stellarindex_external_poller_polls_total{outcome="error"}[15m])
  /
  ignoring(outcome) (
    sum without (outcome) (
      rate(stellarindex_external_poller_polls_total{outcome=~"success|error"}[15m])
    )
  )
) > 0.5
```

- Denominator is success + error only; `outcome="skipped"` (`internal/sources/external/runner.go`: cooldown reached, or oracle with no new round) is excluded so a post-429 cooldown does not dilute the ratio.
- `sum without (outcome)` + `ignoring(outcome)` is required: a bare `success + error` sum matches nothing under one-to-one matching (`outcome` differs) and could never fire. Reproduce by hand with the expression above, not the unaggregated form.
- A genuine trip needs ~30 min of degradation (15 m windows + `for: 15m`).

Impact depends on source class (`registry.go`): exchange-class venues (`binance`, `kraken`, `bitstamp`, `coinbase`, `exchangeratesapi`) are `IncludeInVWAP: true` (loss degrades the aggregate for their pairs; `/v1/price` keeps serving from the remaining sources); aggregator-class (`coingecko`, `coinmarketcap`, `cryptocompare`) and `ecb` are `IncludeInVWAP: false` (degrades cross-checks / FX sanity only).

Diagnose:

```sh
# Which source(s) are erroring + at what rate
curl -s 'http://localhost:9090/api/v1/query?query=sum%20by%20(source)%20(rate(stellarindex_external_poller_polls_total%7Boutcome%3D%22error%22%7D%5B15m%5D))'
# The actual error (pollers live in the indexer)
journalctl -u stellarindex-indexer -n 500 --no-pager | grep -iE 'poller error.*source=' | tail -20
# Manual probe of the vendor endpoint (BASE/QUERY per internal/sources/external/<vendor>/)
curl -sv 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd' 2>&1 | head -20
```

Signals: `/v1/sources?include=stats` shows the vendor with a stale `last_event_unix`; HTTP 429 = rate limit (compare cadence to the vendor cap, upgrade tier); 401/403 = key rotated/revoked (env var per `internal/sources/external/<vendor>/poller.go`; CoinGecko 403 often means the public tier was denied); 5xx = vendor outage; connect timeout = DNS/egress, and if every poller errors at once it is a host problem ([host-down](infra.md#stellarindex_host_down) / [all-ingestion-down](all-ingestion-down.md)); schema parse error = vendor changed shape (code update).

### CoinGecko 429 pattern

The poller (`internal/sources/external/coingecko/poller.go`) has cooldown handling: exponential backoff from `MinBackoff = 60s` to `MaxBackoff = 1h`, honours `Retry-After`, and treats 403 like 429 (CoinGecko returns 403 when the public tier is denied). Keys are env-wired (above), not TOML fields; Pro wins when both are set.

| Tier | Cap | Symptom | Action |
|---|---|---|---|
| Public (no auth) | ~5-15 req/min, IP-throttled | HTTP 403, no `Retry-After`; 60s cooldown, doubling per consecutive denial | free demo key from coingecko.com/api/pricing, set `COINGECKO_DEMO_API_KEY=` in `/etc/default/stellarindex`, restart the indexer |
| Demo (free) | 30 req/min, 10,000 calls/day | HTTP 429, sometimes `Retry-After`; same backoff | check the daily budget below first |
| Pro (paid) | 500 req/min (Analyst) -> 1000 (Pro) -> custom (Enterprise) | 429 only past the paid cap; rare | check status.coingecko.com; if clear, raise the poll interval until the next billing cycle |

Daily budget (F-0030; every caller batches):

| Caller | Shape | Calls/day |
|---|---|---|
| `coingecko` poller (indexer) | ONE batched `/simple/price?ids=…&vs_currencies=…` per tick, `DefaultPollInterval = 300s` | ~288 |
| divergence CoinGecko reference (aggregator + API) | ONE batched `/simple/price` per tick burst (`batchTTL` 25 s); the divergence pass is gated by `divergence_min_interval_seconds` (default 300) | ~288 per binary |
| divergence supply reference (`/coins/{id}`) | off by default (needs a Pro key) | 0 |

That is well inside 10,000/day, so a 429 is not catalogue growth. It is one of: (1) no CG key (`auth_mode=anonymous` on the startup line); (2) several binaries sharing one key AND one egress IP (indexer, aggregator, API; an ad-hoc `stellarindex-ops verify-external` run counts too; run on demand, not in a loop); (3) a CoinGecko incident.

```sh
# Manual probe with the SAME key the binary uses (demo key travels as a header)
curl -sv -H "x-cg-demo-api-key: KEY" \
  "https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd" 2>&1 | head -20
# Backoff state is in the poll error itself
journalctl -u stellarindex-indexer -n 500 --no-pager | \
  grep -iE 'coingecko.*(throttled|backing off)' | tail -5
```

Mitigation:
- [ ] 429: raise `[external.<vendor>] poll_interval` in `/etc/stellarindex.toml`, `systemctl restart stellarindex-indexer`.
- [ ] 401/403: rotate/provision the key in `/etc/default/stellarindex` and restart the owning binary; confirm the tier on the `external poller enabled … auth_mode=` line.
- [ ] Vendor outage: nothing to do (`/v1/price` serves from the remaining sources).
- [ ] Schema drift: code update (release per [release-process](../release-process.md)). Parse path: streaming CEX venues in `internal/sources/external/<vendor>/parse.go` (binance, bitstamp, coinbase, kraken); poller-only vendors (coingecko, ecb, cryptocompare, coinmarketcap, exchangeratesapi) decode inline in `poller.go`. External venues have no `dispatcher_adapter.go`.
- [ ] Verify: ratio < 0.5; allow the 15 min window plus `for: 15m`.

Postmortem capture: 24 h poller log for the vendor, the vendor status page at fire time, the parse path diffed against the captured response if schema drift is suspected.

False positives: vendor maintenance windows under ~30 min are absorbed; a source polled hourly has few samples per 15 min window so one error can exceed 0.5 (check absolute counters). Related: [aggregator](aggregator.md#stellarindex_aggregator_fx_snap_fallback_dominant) (FX vendor failures pushing to the snap fallback).

## stellarindex_chainlink_feed_stale

Trips: `(time() - stellarindex_chainlink_feed_last_success_unix) > 1800`, `for: 5m`, `severity: ticket` (P3). One Chainlink feed (`pair` label) wrote no current round to `oracle_updates` for 30+ min while siblings and `stellarindex_external_poller_stale` may stay green (the framework marks a tick `error` only when no feed produced an update).

`stellarindex_chainlink_feed_last_success_unix{pair}` advances only when the feed returned a round within its `max_age_hours`; it is seeded to 0 at startup, so a feed that never succeeds after a restart also fires. Outcomes are counted on `stellarindex_chainlink_feed_polls_total{pair,outcome}`. Refused rounds are never written: older than the budget (`stale`) or `answeredInRound < roundId` (`carried_forward`) would land as a new observation of a price not published then. `stellarindex_oracle_stale{source="chainlink"}` may follow ([oracle-stale](divergence.md#stellarindex_oracle_stale)).

Diagnose: log `chainlink feed poll failed … pair=<pair> outcome=<outcome>`; then

```promql
sum by (outcome) (increase(stellarindex_chainlink_feed_polls_total{pair="<pair>"}[15m]))
```

| Outcome | Meaning | Action |
|---|---|---|
| `stale` | proxy `updatedAt` older than the budget: feed paused/deprecated/retired, or budget tighter than the heartbeat | check the feed on data.chain.link; if retired remove it from `[external.chainlink].feed_map` or move to the replacement proxy; if the heartbeat changed set `max_age_hours` from it. Never widen the budget just to silence the alert |
| `carried_forward` | aggregator carried an old answer forward | usually transient; if persistent treat as `stale` |
| `error` | RPC, decode, decimals refusal or projection failure | read the WARN `err`; decimals refusal: [chainlink-feed-decimals](divergence.md#stellarindex_chainlink_feed_decimals_mismatch); RPC: `stellarindex_external_poller_stale` |
| only `0` samples | gauge seeded, no poll ran for the pair | check the indexer ticks chainlink: `stellarindex_external_poller_polls_total{source="chainlink"}` |

Clears on the first poll returning a round within budget. A retired feed stays dark until removed from config; correct, the last round must not be re-served as current. Code: `internal/sources/external/chainlink/poller.go` (`checkRoundCurrent`, `recordFeedOutcome`).

## stellarindex_external_fx_feed_stale

Trips: `(time() - max(stellarindex_external_fx_last_quote_unix)) > 21600` (6 h), `for: 15m`, `severity: ticket` (P2). The freshest gauge across all FX sources is > 6 h stale: the hourly `massive` (massive.com) forex worker (in the API binary) stopped committing fresh upstream rates.

The gauge advances only on a committed `fx_quotes` write carrying at least one fresh upstream rate; the synthetic USD=1.0 anchor row and carried-forward history bars (rewritten every refresh) do not count. It is stamped in `persistSnapshot` (`internal/sources/external/forex/worker.go`; metric `ExternalFXLastQuoteUnix` in `internal/obs/metrics.go`).

Impact: none yet. The forex-snap (`Store.FXQuoteAtOrBefore`, lookback `fxQuotesSnapLookback` = 7 days in `internal/storage/timescale/fx_quotes.go`) prices every fiat-quoted pair (XLM/EUR, …) off the last-good `fx_quotes` row; unfixed, fiat pairs break silently when the 7 days expire. This alert exists to fix the feed before that cliff (poller staleness alerts cannot see it).

Diagnose:

```sh
# the feed runs in the API binary, not the indexer
ssh root@136.243.90.96 \
  'journalctl -u stellarindex-api --since "8 hours ago" --no-pager \
    | grep -E "forex: (fx_quotes persisted|rates fetch failed|names fetch failed|fx_quotes persist failed|primary failed|fallback failed)"'
# credential present? (empty key => every fetch 401s, no row ever written)
ssh root@136.243.90.96 \
  'grep -c MASSIVE_API_KEY /etc/default/stellarindex || echo "KEY NOT SET"'
```

Healthy: one `fx_quotes persisted` line per hour (r1 baseline ~803 rows/tick).

| Log line | Stage | Likely cause |
|---|---|---|
| `forex: rates fetch failed` | upstream HTTP | 429 / 401 / subscription lapse / 5xx |
| `forex: names fetch failed` | upstream HTTP | same (names endpoint) |
| `forex: fx_quotes persist failed` | DB write | Postgres down, `fx_quotes` migration missing ([fx-history-missing](fx-history-missing.md)), disk full |
| no forex lines | worker not running | crashed / dry-run / not wired |

Fix, `massive` is a PAID feed (429 or lapsed subscription shows as `forex: rates fetch failed` with `http 401/403/429`):
- [ ] Check the massive.com dashboard (subscription / quota).
- [ ] Key rotated/lapsed: update `MASSIVE_API_KEY` in the r1 env file, `systemctl restart stellarindex-api`.
- [ ] Verify: seconds after restart the log shows `forex: fx_quotes persisted` and the gauge re-stamps; the alert clears at the next evaluation.

Standby while `massive` is dry: the worker retries against the ECB daily reference rates (`forex.ECBProvider`, `internal/sources/external/forex/fallback.go`, registered with `WithFallbacks` in `cmd/stellarindex-api/main.go`). Always on, nothing to enable. Rows go to `fx_quotes` with `source = 'ecb'`; the snap read does not filter on source, so ECB rates price fiat pairs. Limits: ~30 currencies, not massive's 111 (pairs outside ECB's list keep their last-good `massive` row until the 7-day lookback expires); one publication per TARGET working day (~16:00 CET), so no intraday moves and none over weekends/holidays. Nothing else serves the snap: `exchangeratesapi` and the registry's `ecb` connector (authority-sanity, outside `FXSources()`) write `oracle_updates` only.

The stale alert takes the freshest series across sources, so while the standby serves, `stellarindex_external_fx_last_quote_unix{source="ecb"}` keeps advancing and the alert stays clear although `massive` is down. A firing alert means the standby is not serving either.
- [ ] Check the standby: `forex: primary failed — serving from fallback` with `fallback=ecb` = serving; `forex: fallback failed` or `forex: rates fetch failed on every source` = down too. The 7-day lookback is then your time budget.
- [ ] Restoring `massive` is still the fix (fewer currencies, one move per working day).
- [ ] If both stay dry past 7 days, escalate: keeping fiat pairs priced needs a code change to the read path in `internal/storage/timescale/trades.go` (`FXQuoteAtOrBefore`), not a config flip.

`openexchangerates` is built but not serving: `forex.OpenExchangeRatesProvider` (`internal/sources/external/forex/openexchangerates.go`), with `[external.openexchangerates] enabled = true`, is handed to the worker via `WithCorroborator`, which only stores it: never fetched, no `fx_quotes` row, no `source` label, so enabling it neither spends quota nor mitigates this alert. App id from `OPENEXCHANGERATES_APP_ID`, sent only as `Authorization: Token …`. Free plan: 1,000 requests/month, hourly updates, refuses `base`/`symbols`; once wired, one request per `[external.massive] refresh_interval` poll spends 720–744/month at the 1h default.

Postmortem capture: forex-worker log from last good write to first failure, massive.com dashboard (quota / subscription / billing), `SELECT ticker, MAX(bucket) FROM fx_quotes GROUP BY ticker`.

False positives: an API restart re-stamps the gauge within seconds (refresh on startup), but if the alert fires right after a deploy confirm the first refresh completed; gaps under 6 h (6 missed hourly cycles) are tolerated by design.

## stellarindex_external_fx_feed_absent

Trips: `absent(stellarindex_external_fx_last_quote_unix)`, `for: 30m`, `severity: ticket`. No series for 30 min: the worker refreshes on startup, so a healthy boot stamps it in seconds; absence means it never succeeded since API start: dead worker, unset/invalid `MASSIVE_API_KEY`, or the `fx_quotes` persist path wedged from the first tick. Same impact and fix as `stellarindex_external_fx_feed_stale`, plus:
- [ ] Confirm the API binary is up and NOT in dry-run mode (dry-run exits before the forex goroutine spawns; `cmd/stellarindex-api/main.go`).
- [ ] Confirm the `fx_quotes` hypertable migration is applied (missing table = every persist fails; [fx-history-missing](fx-history-missing.md)).

## stellarindex_external_fx_rate_rejections

Trips: `sum by (reason) (increase(stellarindex_external_fx_rate_rejected_total{reason!~"history_deviation_stuck|deviation_history_conflict_stuck"}[3h])) > 2`, `for: 30m`, `severity: ticket` (P3). Tickers are HELD on their last accepted `fx_quotes` row, so their fiat-quoted `usd_volume` converts at a stale rate. No wrong number is published (the guard refuses to); the held rate just ages.

`internal/sources/external/forex/worker.go` gates every current rate through a sanity band (`maxRateDeviation`, 50%) before writing `fx_quotes`, counting refusals on `stellarindex_external_fx_rate_rejected_total{source,reason}`. `fx_quotes` is the denominator of every fiat-quoted `usd_volume`; one bad bar (decimal shift, unit-scale change, upstream redenomination) would re-scale a currency's whole history. Two-strike: an out-of-band rate is held on first sighting and accepted if the NEXT fetch agrees (a genuine devaluation costs one refresh interval). The worker refreshes hourly, so `> 2 in 3h` for 30 min means the upstream keeps disagreeing and confirmation is not clearing it.

`reason`:
- `deviation`: moved > 50% from the last accepted rate, not yet confirmed by a second fetch.
- `non_positive`: rate `<= 0` (empty upstream field; `1/rate` feeds `InverseUSD`, so it would poison both directions).
- `non_finite`: NaN or ±Inf.
- `history_deviation`: a trailing-7d HISTORY bar moved > 50% from the ticker's current accepted rate (past bars are banded read-only, never move the baseline).
- `history_deviation_stuck`: same (within 1%) history bar refused >= 12 consecutive times; excluded from the alert, still WARN-logged. >= 4 mutually-agreeing rejected bars against a still-unconfirmed bootstrap baseline instead HEAL the baseline (`stellarindex_external_fx_baseline_healed_total`; history-majority heal in `forex/worker.go`).
- `deviation_history_conflict`: a two-fetch confirmation was VETOED because the ticker's trailing-7d history majority (>= 4 bars agreeing within 10%) refutes the candidate (two repeats of one broken current bar are not corroboration). History never SETS the baseline, it only refuses the confirm; a genuine devaluation confirms once the trailing majority stops refuting: the history follows a real devaluation within days, or the split-level window fails mutual agreement and yields no veto. Log: `forex: pending confirmation refuted by agreeing history majority`.
- `deviation_history_conflict_stuck`: same (within 1%) vetoed candidate refused >= 12 consecutive times; excluded from the alert, WARN-logged, graphable.

Diagnose:
1. Which ticker (the metric has no ticker label, cardinality): `journalctl -u stellarindex-api | grep 'forex: rejected upstream rate'`; each line carries `ticker`, `rate` (refused), `previous` (last accepted), `reason`, `band`.
2. Is the move real? Compare `rate` to an independent source (ECB reference rates or any public quote). A redenomination or managed-float break is real; a clean power of ten between `rate` and `previous` is an upstream decimal shift.
3. Whole feed or one ticker? `non_positive` / `non_finite` across many tickers = upstream shape change: check `stellarindex_external_fx_feed_stale` and the `massive` subscription.

Fix:
- Real move the upstream keeps reporting: nothing in code; the two-strike arm accepts after two agreeing fetches. If it still fires, the upstream oscillates between scales (feed bug): report it and consider pinning the currency out of the fiat-quote surface.
- Decimal shift / unit-scale change upstream: guard did its job, no `fx_quotes` row corrupted, nothing to backfill; open an upstream ticket.
- A bad bar reached `fx_quotes` before the band shipped (historical rows): rows are per `(ticker, bucket)`; correct with an `InsertFXQuoteBatch`-equivalent upsert for the day, then re-derive fiat-quoted aggregates over the window.
- Band too tight: `maxRateDeviation` is a documented constant; changing it is a code change with a test, not a toggle.
- Do NOT widen the band to silence the alert, do NOT disable the guard (confirmation already lets a real rate through on the next refresh), and do not read a firing alert as wrong data: data was withheld.

Known upstream defects (check before deep-diving):
- ETB history bar = 44 (Massive): the trailing-7d refresh serves 44 against a held baseline ~161.75 (log `ticker=ETB rate=44 previous=161.75`; 44 is Ethiopia's pre-float rate; real ~155–165/USD). Only the history bar is refused (current path unaffected). After 12 consecutive refusals of the same value the reason becomes `history_deviation_stuck` (excluded from the alert; graphable on `…rejected_total{reason="history_deviation_stuck"}`); a new value, new ticker or current-rate rejection alerts immediately. If the CURRENT ETB rate starts rejecting, treat as a fresh incident. Consider reporting to Massive.
- UZS current bar ~1820 (jitter ±1%) against a true ~11,800 (Massive): broken current endpoint. The restart-heal fixes the poisoned bootstrap baseline and the confirm veto (`deviation_history_conflict`) stops the repeating broken value from confirming back in; after 12 refusals it becomes `deviation_history_conflict_stuck`. UZS is held on its last accepted row (stale, never wrong). Report to Massive if it persists.

## stellarindex_fx_fixings_refresh_stale

Trips: `(time() - max(stellarindex_fx_fixings_last_refresh_unix)) > 10800` (3 h), `for: 15m`, `severity: ticket`. The `fx_fixings` appender has not completed a cycle in 3 h. A closed fiat-cross bucket (XLM/EUR at a past minute) converts at the `fx_fixings` bar with the greatest `bar_end` at least 3 h before the bucket end; with no such bar inside the cross max age it is withheld with `price_withheld_reason: fx_leg_unavailable`, never converted at a later rate. Unlike `fx_quotes` there is no 7-day cushion.

The appender runs at the end of every forex refresh, after the cache install: check the forex worker first (`stellarindex_external_fx_feed_stale` diagnosis).

## stellarindex_fx_fixings_series_stale

Trips: `(time() - max(stellarindex_fx_fixings_newest_bar_end_unix)) > 14400 and on() ((day_of_week() >= 2 and day_of_week() <= 5) or (day_of_week() == 1 and hour() >= 2))`, `for: 15m`, `severity: ticket`. Cycles complete but the newest stored bar is > 4 h old on a trading day (Tue–Fri, or Mon from 02:00 UTC); closed fiat crosses are withheld or `fx_stale`.

- [ ] Grep API logs for `fx_fixings bar fetch failed` (vendor error per ticker) and `fx_fixings insert failed` (DB); counted by `stellarindex_fx_fixings_fetch_errors_total` and `stellarindex_fx_fixings_write_errors_total`.
- [ ] Newest bar per ticker: `SELECT ticker, MAX(bar_end) FROM fx_fixings GROUP BY ticker ORDER BY 2`.
- [ ] Refill a gap once the vendor is back (idempotent; the live appender re-offers only the last 48 h): `go run ./scripts/ops/fx-history-backfill --series=fixings --from=YYYY-MM-DD --ticker=EUR,GBP`.

## stellarindex_fx_fixings_quote_disagreement

Trips: `max by (ticker) (stellarindex_fx_fixings_quote_disagrees) == 1`, `for: 2h`, `severity: ticket`. An accepted hourly fixing sits outside [0.5, 1.5] of the guarded daily rate for its day: the hourly and daily series disagree about one currency. Bars pass their own gate before insert: a close outside [0.5, 1.5] of the median of the <= 24 previous vendor bars in the prior 96 h is refused (`stellarindex_fx_fixings_bars_refused_total`, log `fx_fixings gate refused bar`).

- [ ] Find the bar: grep API logs for `fx_fixings bar disagrees` (ticker, `bar_start`, close, guarded rate).
- [ ] Decide which series is wrong against an independent source (ECB, the OXR corroborator gauge). Wrong daily rate: `stellarindex_external_fx_rate_rejections` above; wrong hourly bar: correct it.
- [ ] Correct a bar by INSERTING the right close for the same `(ticker, grain, bar_start)` at generation `max + 1` (readers take the highest generation). Never `UPDATE` or `DELETE` an `fx_fixings` row: it is the record of what every served closed cross converted at. Record the bar, affected window and evidence in the incident notes. For a range: `go run ./scripts/ops/fx-history-backfill --series=fixings --generation=N --from=YYYY-MM-DD --ticker=UZS`.

## stellarindex_cex_stream_subscription_rejected

Trips: `max by (source, symbol) (stellarindex_cex_stream_subscription_rejected) == 1`, `for: 5m`, `severity: ticket`. The venue answered our subscribe for `symbol` with `success:false`: that pair delivers no trades while the socket looks healthy (no reconnect, stall or decode error) and the aggregator's `source_count` for it drops by one. The streamer does NOT reconnect (the venue answers per symbol; other pairs stay live). Gauge set by kraken (`internal/sources/external/kraken/streamer.go::frameHandler`) and bitstamp (`bitstamp/streamer.go`).

Diagnose: indexer log `kraken rejected the trade subscription` (or `bitstamp rejected the trade subscription`); `venue_error` field is the venue text (e.g. `Currency pair not supported XLM/GBP`).

Fix: rejected or renamed pair: update `DefaultPairList` in `internal/sources/external/kraken/pairs.go` (bitstamp: `bitstamp/pairs.go`) and deploy. The gauge clears to 0 when the venue next acknowledges the symbol; a symbol removed from config keeps its last value until the indexer restarts; `symbol="unknown"` (rejection named no configured symbol) also clears only on restart.

## stellarindex_cex_stream_entry_skips

Trips: `sum by (source, reason) (rate(stellarindex_cex_stream_entry_skips_total[15m])) > 0`, `for: 30m`, `severity: ticket`. Trade entries inside well-formed frames are being dropped continuously for 30 min (kraken streamer). `reason`: `unknown_symbol`, `bad_qty`, `bad_price`, `bad_timestamp`, `bad_trade_id`, `other`.

Diagnose: log `kraken trade entry skipped` (once per reason per minute; `err` names the symbol or field value).
- `unknown_symbol`: venue sends a symbol not in `internal/sources/external/kraken/pairs.go::DefaultPairList`, usually a rename of a configured pair; update it.
- `bad_qty` / `bad_price` / `bad_timestamp`: the venue changed a field's encoding (scientific notation, epoch timestamps); compare the logged value with `internal/sources/external/kraken/parse.go::buildTrade` and fix it with a test pinned to the new wire shape.

Whole-frame decode failures are a different signal: [decode-errors](decode-errors.md) (`stellarindex_source_decode_errors_total`).

## stellarindex_cex_stream_stalled

Trips: `sum by (source) (increase(stellarindex_cex_stream_disconnect_total{reason="stall"}[15m])) > 0`, `for: 5m`, `severity: ticket` (P3). `wsclient.ErrStreamStalled`: the venue stopped answering pings but TCP has not noticed (half-open socket), a silent gap in trade coverage. Typical MTTR 5–30 min.

```sh
# Which source and reason is firing? (indexer metrics port)
curl -fs http://localhost:9464/metrics | grep -E '^stellarindex_(cex_stream_disconnect_total|external_dust_dropped_total)'
journalctl -u stellarindex-indexer | grep -E 'stream stalled' | tail -20
```

Fix: the reconnect loop (`wsclient.Loop`, `internal/sources/external/wsclient/`) already forces a fresh dial on `ErrStreamStalled`. If the rate stays elevated, suspect venue-side infra (a stuck load-balancer node) rather than our client; restarting the indexer forces a fresh outbound connection immediately. Verify the counter's rate returns to baseline.

## stellarindex_external_dust_dropped_high

Trips: `sum by (source) (rate(stellarindex_external_dust_dropped_total[15m])) > 1`, `for: 30m`, `severity: ticket` (P3). More than 1 trade/s dropped as sub-$0.001 dust (floor resolved per quote asset, `forwardTrade` in `internal/sources/external/runner.go`), sustained 30 min; real volume may be thrown away.

Diagnose: same metrics curl as `stellarindex_cex_stream_stalled`; compare against the venue's own trade feed for the same window.
- Legitimate sub-cent fills spiked (low-price/high-volume listing event, e.g. a new sub-cent token): expected; cross-check the venue's volume dashboard, then acknowledge.
- Otherwise check whether the venue changed a symbol's price/amount scaling: the dust guard keys off quote/base ratio, so a decimals regression reads as "everything is dust". Never assume scaling is uniform (see `Decimals` per source). Verify the rate returns to baseline.

## Related

- [Alerts catalogue](../alerts-catalog.md)
