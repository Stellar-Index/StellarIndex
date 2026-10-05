---
title: Runbook — divergence and oracle alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — divergence, oracle staleness and Chainlink decimals

Rule file: `configs/prometheus/rules.r1/divergence.yml` (what r1 loads, group `stellarindex.divergence`); `deploy/monitoring/rules/divergence.yml` is the multi-host twin and is identical. All alerts are `severity: ticket` except the 5 % divergence tier (`informational`). Impact is bounded: aggregate prices keep serving; only the divergence flag, `/v1/oracle/*` or a Chainlink feed is degraded.

Aggregator metrics are on `localhost:9465`, indexer metrics on `localhost:9464`. r1: `ssh root@136.243.90.96`.

## Reading the divergence flag

`flags.divergence_warning` on `/v1/price` is `divergence.CachedResult.WarningFired`, cached per pair at the Redis key `div:<base>/<quote>` and ORed across every quote of the base. It is true only when ALL of:
1. at least `min_sources_for_warning` references answered (`success_count`, default 2);
2. EITHER the references' median differs from our price by more than `threshold_pct` (default 5), OR no answering reference agrees with us within it (`agreement_count` = 0; symmetric disagreement leaves the median on our price);
3. that raw condition has persisted for `WarningPersistence` (default 5 min) across at least two refreshes.

To answer "is the flag on, and why": `redis-cli GET 'div:<base>/<quote>'` and read `warning_fired`, `success_count`, `agreement_count`, `divergence_pct`.

`divergence_observations` rows are NOT the flag. The worker writes one row per answering reference per refresh, `status = 'firing'` whenever that single reference's `|delta_pct|` exceeds the threshold, with no quorum, median or debounce. Firing rows with the flag off are normal; never infer the flag from the rows. There is one worker threshold; the 5 % / 10 % tiers are the Prometheus rules on `stellarindex_divergence_max_abs_fraction`.

Reference set (`divergence_observations.reference`: migration 0019, widened by 0148, mirrored in `internal/api/v1/anomalies.go::divergenceReferences`): `chainlink`, `coingecko`, `reflector-cex`, `reflector-fx`, `reflector-dex`, `redstone`, `band`, `synthetic-usd-cross` (derived XLM/USD × USD/fiat cross so EUR/GBP-quoted pairs reach the trust floor). CoinMarketCap is NOT a divergence reference (there is only a disabled-by-default venue poller, `internal/sources/external/coinmarketcap`).

The oracle references (Reflector dex/cex/fx, Redstone, Band) are default ON and read our own ingested `oracle_updates` rows (zero external quota, `internal/divergence/oracle.go`); a frozen feed there is an ingestion question ([`oracle_stale`](#stellarindex_oracle_stale)). The aggregator startup log line `divergence refresher wired` lists exactly which references are live; read it before assuming which upstream is involved.

Refresh cadence: the orchestrator calls `divergence.Service.RefreshPair` per configured pair, rate-limited by `[aggregate] divergence_min_interval_seconds` (default 300 s; the 30 s Tick still fires but the pass is skipped while elapsed < the interval). `div:<asset>` TTL is the refresh cadence plus one worst-case pass, never under 5 min. Pass outcomes on `stellarindex_divergence_refresh_total{outcome}`: `ok`, `refresh_error`, `no_reference`, `no_vwap`, `parse_error`. Code: `internal/aggregate/orchestrator/divergence_refresh.go`, `internal/divergence/`.

## At a glance

- [`stellarindex_price_divergence_warning`](#stellarindex_price_divergence_warning)
- [`stellarindex_price_divergence_critical`](#stellarindex_price_divergence_critical)
- [`stellarindex_oracle_stream_rows_unparsed`](#stellarindex_oracle_stream_rows_unparsed)
- [`stellarindex_oracle_stale`](#stellarindex_oracle_stale)
- [`stellarindex_divergence_refresh_error_dominant`](#stellarindex_divergence_refresh_error_dominant)
- [`stellarindex_divergence_no_reference`](#stellarindex_divergence_no_reference)
- [`stellarindex_divergence_no_ok_outcomes`](#stellarindex_divergence_no_ok_outcomes)
- [`stellarindex_divergence_reference_failing`](#stellarindex_divergence_reference_failing)
- [`stellarindex_divergence_pair_below_quorum`](#stellarindex_divergence_pair_below_quorum)
- [`stellarindex_chainlink_feed_decimals_mismatch`](#stellarindex_chainlink_feed_decimals_mismatch)
- [`stellarindex_chainlink_feed_decimals_verify_failed`](#stellarindex_chainlink_feed_decimals_verify_failed)

## stellarindex_price_divergence_warning

Trips: `stellarindex_divergence_max_abs_fraction > 0.05`, `for: 10m`, `severity: informational`. Shared with `_critical`: the gauge is the worst `|ours - ref| / ref` per `{reference}` (bounded, no per-asset label); `stellarindex_divergence_pairs_over{threshold="5pct"}` / `{threshold="10pct"}` counts the pairs and `divergence_observations` names them. The gauge compares a shortest-window VWAP to instantaneous quotes, so `for: 10m` absorbs the lag on a fast move. Normal during rapid market moves.

Diagnose (> 100x divergence is always a decoder bug, never a market move; wrong decimals give 100x or 1e6x, not 5-10 %):

```sh
# Which asset + reference are firing right now, and by how much?
psql -d stellarindex -c \
  "SELECT asset_id, quote_id, reference, our_price, ref_price, delta_pct, observed_at
   FROM divergence_observations
   WHERE status = 'firing'
     AND observed_at > now() - interval '15 minutes'
   ORDER BY abs(delta_pct) DESC LIMIT 10;"

# Compare our live price against the reference (XLM is `native`; API on :3000).
curl -s 'http://localhost:3000/v1/price?asset=native&quote=fiat:USD'
curl -s 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'

# What sources contribute to our aggregate for this asset?
psql -d stellarindex -c \
  "SELECT source, count(*) AS trades, max(ts) AS latest
   FROM trades
   WHERE (base_asset IN ('native', 'crypto:XLM')
          OR quote_asset IN ('native', 'crypto:XLM'))
     AND ts > now() - interval '1 hour'
   GROUP BY source ORDER BY trades DESC;"
```

Causes, roughly by severity:
1. **Stale source in the aggregate.** One source's `max(ts)` is far behind the others and its price is > 5 % off the market. A stopped source's trades keep contributing for the aggregation window (by design), so divergence persists until the window rolls. Cross-check [`source-stopped.md`](source-stopped.md); if chronically stopped, disable it in `ingestion.enabled_sources`.
2. **Wrong decimals.** ADR-0003: an `int64(parts.Lo)` truncation in a decoder gives a price off by 2^N (ratio > 100x or ~0 for the whole asset on one source); golden-file tests in `internal/sources/*/` should catch it. Revert the decoder PR, audit trades ingested with that version, and delete them from the `trades` hypertable before they poison aggregates. This is a data-integrity incident.
3. **Illiquid market / real price discovery.** Low trade count, no other sources, no sibling asset from the same issuer diverges. A real signal, not an incident; demote with a label-specific silence if sustained.
4. **Reference broken** (CoinGecko outage or 429, Chainlink stale feed, frozen Reflector/Redstone/Band feed): every asset diverges from that one reference while the others agree. Disable that reference in `[divergence]` config and restart aggregator + API. If ALL references go dark for a pair, see [`stellarindex_divergence_no_reference`](#stellarindex_divergence_no_reference).
5. **Arbitrage window / CEX halted while the DEX keeps moving** (CoinGecko's aggregate lags). Real divergence, not our fault.

Fix: eyeball the order of magnitude, identify the contributing sources and any stale one, then decoder bug -> revert, audit, purge, redeploy (becomes a postmortem); stale source -> mitigate via `source-stopped.md`; real move -> record in `postmortems/`. Verify divergence drops under 5 % (10 % for critical) and the alert clears. Freshly listed tokens have no reference price (issuer-advertised or empty); exclude them from the divergence feed for their first 24 h. For the RCA capture asset + reference + window, contributing sources with trade counts and latest observations, raw trade rows in the window, and whether two other references agree. ADR-0003 is the decoder discipline that prevents decimal divergence.

## stellarindex_price_divergence_critical

Trips: `stellarindex_divergence_max_abs_fraction > 0.10`, `for: 10m`, `severity: ticket`. Same gauge, diagnosis and fix as [`stellarindex_price_divergence_warning`](#stellarindex_price_divergence_warning); > 10 % is usually a bad ingest (wrong decimals, a stale source contributing) rather than a market move, so go straight to causes 1-2 there.

## stellarindex_oracle_stream_rows_unparsed

Trips (`for: 10m`, `severity: ticket`):

```promql
increase(stellarindex_oracle_stream_rows_unparsed_total[6h]) > 0
or (
  stellarindex_oracle_stream_rows_unparsed_total > 0
  unless stellarindex_oracle_stream_rows_unparsed_total offset 6h
)
```

The counter `stellarindex_oracle_stream_rows_unparsed_total{source,field}` is deliberately not pre-seeded; the second arm catches a child born inside the window whose first increment `increase()` cannot see, and goes quiet once the birth leaves the window.

Means: `LatestOracleStreams` could not parse the stored canonical `field` text (`asset`/`quote`) on some `<source>` `oracle_updates` rows, so those rows are ABSENT from `/v1/oracle/streams` and the explorer `/oracles` page. Likeliest cause: an operator-run raw SQL `UPDATE` relabelling a mislabelled oracle row (the column has no `CHECK` constraint), so a typo silently deletes the row from the served surface.

Diagnose (the labels name the column and oracle):

```sql
SELECT DISTINCT source, asset, quote
  FROM oracle_updates
 WHERE source = '<source>'
 ORDER BY 1,2,3;
```

Compare against the forms `internal/canonical` accepts: `native`; classic `USDC-GA5ZSEJ…` (code, dash, 56-char G-strkey); Soroban `CA…` (56-char C-strkey); `fiat:USD`; `crypto:BTC`; unmapped oracle symbol `raw:<symbol>`. Likeliest mistakes: truncated or lower-cased strkey; missing prefix (`USD` for `fiat:USD`); stray space or quote from a shell-built `UPDATE`; `rwa:`/`raw:` confusion.

Fix:
1. Correct the row with a **parameterised** update, not string concatenation.
2. Re-read `/v1/oracle/streams` and confirm the row is back; the alert clearing only means no new drops in the window.
3. If the text is correct and the parser rejects it, a canonical form changed under the running binary: check the deployed version against `internal/canonical` before editing data.

A burst right after a deliberate schema/namespace migration is expected; if it does not stop, the rewrite missed rows. Related: [`oracle-unknown-symbols.md`](oracle-unknown-symbols.md) (a symbol that parses but maps to no canonical asset).

## stellarindex_oracle_stale

Trips (`for: 2m`, `severity: ticket`; a bare comparison, keep it bare):

```promql
(time() - stellarindex_oracle_last_update_unix)
  > stellarindex_oracle_staleness_budget_seconds
```

Both gauges carry `{source, asset}` and are emitted by the same call, so they pair with no matching modifier. If a budget ever lands on a different label set, fix the emitter; do not add `on()`/`ignoring()` (it breaks on duplicate `{source}` series from a second indexer host). Budget default: 10 × the source's declared resolution (`obs.OracleStaleBudgetMultiplier`, `internal/obs/oracle_staleness.go`; > 50 min for Reflector's 5-min cadence), heartbeat sources (RedStone) default to heartbeat + 2 h = 26 h, per-pair exceptions in `[[oracle.staleness_overrides]]`. Impact: `/v1/oracle/latest` for that asset ages; consumers of oracle prices (triangulation, divergence) get stale inputs. Oracles NEVER contribute to VWAP (`internal/sources/external/registry.go` includes only `ClassExchange`), so impact is bounded to `/v1/oracle/*` and divergence references. Band's hourly cadence also sets the daily/hourly publisher windows of the ingestion source-stopped alerts.

Alert label `source` is an on-chain oracle (`reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`, `band`) or an external poller (`chainlink`, `coingecko`, `coinmarketcap`, `cryptocompare`, `ecb`, `exchangeratesapi`). Pollers have no on-chain events: read `stellarindex_external_poller_polls_total{source=…}` and `stellarindex_external_poller_last_success_unix` instead of the event rate. The synchronous Chainlink cross-check in `internal/divergence/` emits no `stellarindex_oracle_*` metrics.

Diagnose:

```sh
# How long since last observation, and what is this pair allowed?
curl -s http://localhost:9464/metrics |
  grep -E "stellarindex_oracle_last_update_unix|stellarindex_oracle_staleness_budget_seconds|stellarindex_oracle_resolution_seconds"

# Is the contract still emitting? No subscription exists to go stale: the
# dispatcher decodes reflector events (topic ["REFLECTOR","update"]) straight
# from the Galexie ledger stream, so a stall on our side is the indexer/
# dispatcher path. Read the ALERTING pair's own budget from the gauge above
# (e.g. crypto:DAI is 9 h); it is not stalled until IT passes ITS bound.
# r1 runs no stellar-rpc; probe a public endpoint:
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
# Contract activity: https://stellar.expert/explorer/public/contract/<contract-id>
```

If `stellarindex_oracle_staleness_budget_seconds` is missing for every asset while `stellarindex_oracle_last_update_unix` is present, the indexer predates the per-asset budget and the alert is blind, not clear: restart onto the current binary. A budget of `+Inf` on one source means it never declared its resolution.

- Fresh updates on-chain but zero in our metrics: indexer/dispatcher stalled (or decoder rejecting events). Restart: `ssh root@136.243.90.96 systemctl restart stellarindex-indexer` (resumes from the persisted cursor, nothing skipped). If events arrive but do not land, check `stellarindex_source_decode_errors_total` (see [`decode-errors.md`](decode-errors.md)) and `stellarindex_source_unknown_symbols_total`; oracle decoders emit `raw:` rows for unrecognised symbols rather than dropping them ([`oracle-unknown-symbols.md`](oracle-unknown-symbols.md)).
- On-chain activity paused: the publisher (Reflector relayer, Redstone DataService, Band's chain-write bot) is down. Check app.reflector.world, app.redstone.finance, data.bandprotocol.com; open an incident with the upstream ETA. The API flags affected prices `stale=true` in the response envelope.
- One asset stops while siblings flow: it publishes on its own rhythm (above) or the contract de-listed it; note the de-listing and move on.
- Multiple oracle sources stale at once: escalate to P1 (every oracle triangulation path is broken). A single stale oracle is usually covered by the other two Reflector variants.
- Verify: `stellarindex_oracle_last_update_unix` for the source advances again.

**First rule out a budget that never matched the asset** (the main false positive). The resolution is a hard-coded constant per source (`DefaultResolutionSeconds` in `internal/sources/<oracle>/events.go`; for a poller its `OracleResolution` in `internal/sources/external/registry.go`, raised to the poll interval when slower) and nothing reconciles it with real cadence. Two shapes: (1) the SOURCE's declared cadence is wrong, so every asset over-fires: fix the constant (`band` once declared 60 against an hourly relay and fired on 100 % of samples; it is 3600 now, pinned by `internal/sources/band/resolution_test.go`; a source without such a test can drift silently); (2) ONE ASSET publishes on its own rhythm (peg assets republish only on movement) and alone over-fires: fix that pair's budget. Tells vs a real stall: intermittent firing over a long time rather than from a point in time, and `/v1/oracle/streams` shows recent observations for the source.

```sh
# Measured cadence PER ASSET over a week (a source average hides the slow member):
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=604800 / changes(stellarindex_oracle_last_update_unix{source="<source>"}[7d])'
# ...against what each pair is allowed:
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_oracle_staleness_budget_seconds{source="<source>"}'
# ...and the source's declared cadence, which seeds the default budget:
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_oracle_resolution_seconds{source="<source>"}'
```

Every asset's measured interval exceeds its budget -> fix the source constant. One asset's does, siblings' don't -> give it its own budget:

```toml
[[oracle.staleness_overrides]]
source         = "reflector-cex"
asset          = "crypto:DAI"     # canonical form, NOT "DAI"
budget_seconds = 32400
reason         = "peg asset — republished only on movement; measured gaps to 7h"
```

in `/etc/stellarindex.toml` (codified in `configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`; `OracleStalenessOverrideConfig` in `internal/config/config.go`). `asset` must be the exact label identifier; the indexer refuses to start on a bare or aliased spelling. Size it from the measured worst gap plus headroom and record the observation in `reason`. An override is not a mute: past the wider bound the pair tickets normally. It takes effect at the next indexer restart and reaches the gauge only on that pair's next publication. The shipped example is `reflector-cex` / `crypto:DAI` at 9 h (gaps reached 7 h against the 50-minute default). Oracle contract IDs are config too: `[oracle.reflector]` (`ReflectorOracleConfig`).

## stellarindex_divergence_refresh_error_dominant

Trips (`for: 30m`, `severity: ticket`; the `and` clause avoids firing at cold start when both rates are 0):

```promql
sum(rate(stellarindex_divergence_refresh_total{outcome="refresh_error"}[5m]))
  >
sum(rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m]))
and
sum(rate(stellarindex_divergence_refresh_total{outcome=~"refresh_error|ok"}[5m])) > 0
```

Means: the refresher is reached but something downstream breaks, so `flags.divergence_warning` stops updating and, once the cache TTL (>= 5 min) lapses, shows no warning even when prices diverge (false-negative window). Aggregator logs repeat `divergence refresh failed` with the error. `no_reference` is neither `refresh_error` nor `ok`, so this alert cannot see an all-references-dark pass.

Diagnose:

```sh
ssh root@136.243.90.96
# 1) Which outcome dominates?
curl -fs http://localhost:9465/metrics | grep '^stellarindex_divergence_refresh_total'
# 2) Which references are wired? (logged once at startup)
journalctl -u stellarindex-aggregator --no-pager | grep 'divergence refresher wired' | tail -1
# 3) The underlying error.
journalctl -u stellarindex-aggregator -n 200 | grep 'divergence refresh failed'
# 4) Probe the references. CoinGecko:
curl -fs 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'
#    Chainlink: the live URL is CHAINLINK_RPC_URL in /etc/default/stellarindex
#    (env overrides the TOML rpc_url; curl it with a JSON-RPC eth_chainId payload).
grep CHAINLINK_RPC_URL /etc/default/stellarindex
```

| Underlying error | Cause | Fix |
| --- | --- | --- |
| HTTP 429 from CoinGecko | The anonymous tier allows ~30 calls/DAY; the reference batches ALL pairs into ONE `/simple/price` call per pass (cached 25 s, `internal/divergence/coingecko.go`) = ~288 calls/day at the 300 s interval. The divergence PRICE reference has NO API-key plumbing (`COINGECKO_API_KEY` reaches only the supply cross-check, `COINGECKO_DEMO_API_KEY` only the CEX poller), so a paid key does NOT help | Lengthen `[aggregate].divergence_min_interval_seconds` or disable the CG reference |
| HTTP 5xx from CoinGecko | Upstream degraded, self-recovers in 5-30 min | Wait; alert auto-resolves |
| Chainlink RPC timeout | Provider issue. TOML default `rpc_url` is `cloudflare-eth.com`; r1's env injects an Alchemy URL (key embedded in the path: treat as a secret) | Check/rotate `CHAINLINK_RPC_URL` in `/etc/default/stellarindex` or disable Chainlink temporarily |
| Redis cache write failed | BGSAVE blocked on a full disk or Redis OOM (r1 runs one local `redis-server`: no Sentinel, no failover to wait out) | [`redis-write-blocked-disk-full.md`](redis-write-blocked-disk-full.md) |
| All references failed | Egress blocked (oracle references excepted: they read local Postgres) | Check egress firewall + DNS on the aggregator host |

Fix: identify the failing reference from the logs and the `divergence refresher wired` line; probe it manually to confirm it is upstream; if recovery is slow, set the relevant `enabled = false` under `[divergence]` and restart the aggregator (the remaining references, including the zero-quota oracle ones, keep feeding the comparison). Verify `rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m])` recovers above the `refresh_error` rate; the alert resolves after 30 min sustained. A fire shortly after a restart is real (refreshes are running and failing). Editing `[divergence]` on a running aggregator causes one pass of `refresh_error`; ignore short blips. For the postmortem capture the failing reference, the error class, FIRING->RESOLVED duration, and whether `flags.divergence_warning` actually went stale for consumers. See `docs/architecture/aggregation-plan.md` and ADR-0019 (divergence in the confidence score).

## stellarindex_divergence_no_reference

Trips (`for: 30m`, `severity: ticket`):

```promql
sum(rate(stellarindex_divergence_refresh_total{outcome="no_reference"}[5m]))
  >
sum(rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m]))
and
sum(rate(stellarindex_divergence_refresh_total{outcome="no_reference"}[5m])) > 0
```

Means: every configured reference is dark for the pair, so `RefreshPair` returns `ErrNoReferenceResponded` and writes a `SuccessCount=0` cache entry. `flags.divergence_warning` freezes at its last value and `divergence_checked` reads `false`; the persistence streak and webhook latch are untouched, so a firing warning is neither cleared nor re-sent when references return. A live depeg during the outage goes unflagged. `ok` and `refresh_error` rates are both near zero; logs show `divergence refresh failed … outcome=no_reference`. Only this alert sees it.

Diagnose:

```sh
# 1) Confirm no_reference (not refresh_error) dominates.
curl -fs http://localhost:9465/metrics | grep '^stellarindex_divergence_refresh_total'
# 2) Which references are wired?
journalctl -u stellarindex-aggregator | grep 'divergence refresher wired' | tail -1
# 3) Probe each reference from the aggregator host:
curl -fs 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'
#   Chainlink: grep 'rpc_url' the aggregator config, curl it with an eth_chainId JSON-RPC payload.
```

Causes:
1. **CoinGecko free-tier 429.** The PRICE reference (`divergence.coingecko`) is free-tier only, with no API key field; there is no key-based fix. If CoinGecko is the only reference covering a pair, expect intermittent `no_reference` until another reference (Chainlink, an on-chain oracle) also covers it. The separate SUPPLY cross-check (`divergence.supply.coingecko`) does take `COINGECKO_API_KEY` (auto-switches to the Pro host); that key has no effect here.
2. **Chainlink RPC dark.** The divergence reference has its own `rpc_url`; confirm `CHAINLINK_RPC_URL` points at a working provider (it feeds ingest and divergence).
3. **Egress blocked.** A firewall/DNS change cut all outbound HTTPS; every reference goes dark at once.

Fix: restore the failing reference (wait out the 429 or add a non-CoinGecko reference for the pair; point `CHAINLINK_RPC_URL` at a live provider); restart the aggregator after any config change. One reference staying down is fine: ANY responding reference clears the alert; only a total outage fires it. Verify `rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m])` recovers; resolves after 30 min sustained.

False positives: cold start (masked by `for: 30m`); a pair no configured reference lists always returns `no_reference` (known-uncovered pair: exclude it, not an outage); a quote observed more than `divergence.MaxComparableAge` before the comparison (1 h; the FX budget for fiat/fiat pairs) is recorded in the cached result's `failures` as `too_stale_to_compare` and does not vote, so a pair covered only by daily-heartbeat feeds (Redstone, Band) reads `no_reference` between their pushes: add a fresher reference.

## stellarindex_divergence_no_ok_outcomes

Trips (`for: 15m`, `severity: ticket`; gated on the refresher being wired, so an operator who disabled every reference is not paged):

```promql
sum(increase(stellarindex_divergence_refresh_total{outcome="ok"}[30m])) == 0
and on()
max(stellarindex_divergence_refresher_wired) == 1
```

Means: the error-dominant and no_reference rules each compare a failure child against `ok`; a pass producing only `no_vwap` (every pair frozen, `min_usd_volume` above real volume, VWAP writer regressed) or no outcome at all satisfies neither. No `div:<asset>` entry has been written for 45+ min, all cached ones expired; `flags.divergence_warning` and `divergence_checked` read false fleet-wide (`/v1/price` carries `divergence_checked: false` for every asset) and the cross-oracle confidence lens has nothing to use.

```sh
# 1) Which outcome IS the pass producing?
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_divergence_(refresh_total|refresher_wired)'
# 2) Are the pairs frozen?
curl -fs http://localhost:9465/metrics | grep '^stellarindex_anomaly_freeze_active'
# 3) Did the pass panic? (the no-vwap line is logged at debug level)
journalctl -u stellarindex-aggregator --since '-1h' | grep -E 'divergence refresh (panicked|: no vwap)' | tail
```

- Every pair frozen (ADR-0019): `no_vwap` climbs at full rate. The freeze is the incident; follow the freeze runbook ([`anomaly-freeze-engaged.md`](anomaly-freeze-engaged.md)); divergence recovers once a fresh VWAP is cached.
- No VWAP in cache with no freeze: `min_usd_volume` raised past real volume, or the VWAP writer regressed. Restore the `[aggregate]` volume floor or fix the writer; confirm `stellarindex_aggregator_vwap_writes_total` climbs.
- Pass dies before counting (every outcome flat): look for `divergence refresh panicked`; capture the stack and restart the aggregator.
- Verify `ok` climbs; resolves on the next evaluation.

False positive: `divergence_min_interval_seconds` above 30 min makes the pass run less often than the 30 m window, so `ok` can read 0 between passes; the default (300 s) never trips it.

## stellarindex_divergence_reference_failing

Trips (`for: 30m`, `severity: ticket`):

```promql
sum by (reference) (rate(stellarindex_divergence_reference_total{outcome!~"ok|asset_unsupported|too_stale_to_compare"}[15m]))
  >
0.5 * sum by (reference) (rate(stellarindex_divergence_reference_total{outcome!~"asset_unsupported|too_stale_to_compare"}[15m]))
```

Means: more than half of one reference's lookups failed (`asset_unsupported` = coverage and `too_stale_to_compare` = feed cadence are excluded from both sides: not outages) for 30+ min. The pass still counts `ok` while any other reference answers, so this is the only signal for a partial outage; a pair covered by exactly `min_sources_for_warning` references then drops below quorum (see [`stellarindex_divergence_pair_below_quorum`](#stellarindex_divergence_pair_below_quorum)).

```promql
# Which outcome is the reference producing?
sum by (reference, outcome) (rate(stellarindex_divergence_reference_total[15m]))
```

`price_unavailable` from CoinGecko is its freshness gate failing closed (missing or stale `last_updated_at`); `timeout` / `overall_deadline_exceeded` is a slow upstream; `panicked` also moves `stellarindex_worker_panics_total`. Remediate the reference as in [`stellarindex_divergence_no_reference`](#stellarindex_divergence_no_reference).

## stellarindex_divergence_pair_below_quorum

Trips (`for: 30m`, `severity: ticket`; `max_over_time` spans ~5 refreshes at the default cadence so one flaky answer does not trip it):

```promql
max by (pair) (max_over_time(stellarindex_divergence_pair_quorum_met[30m])) == 0
```

Means: every refresh of the pair for 60+ min had fewer responding references than `min_sources_for_warning`. Below quorum the warning verdict is carried forward, never re-evaluated, so a live depeg on the pair cannot warn while the pass outcome stays `ok`; `divergence_checked` reads false.

```promql
# Which pairs are disarmed right now?
stellarindex_divergence_pair_quorum_met == 0
```

Find the dropped reference with `stellarindex_divergence_reference_total` by `outcome` and remediate as for `reference_failing`. A pair whose only other coverage is a daily-heartbeat feed sits below quorum between pushes: structural, not an outage; add a fresher reference.

## stellarindex_chainlink_feed_decimals_mismatch

Trips (`for: 5m`, `severity: ticket`):

```promql
sum by (consumer, pair) (increase(stellarindex_chainlink_feed_decimals_mismatch_total[15m])) > 0
```

Both Chainlink readers (`internal/divergence` cross-check, `internal/sources/external/chainlink` ingest poller) verify a feed's configured `decimals` against the AggregatorV3 proxy's on-chain `decimals()`, else every reading would be scaled by `10^(configured-actual)` silently. Mismatch fails CLOSED: the `{consumer, pair}` feed is refused (`ErrPriceUnavailable`) until they agree, producing no divergence cross-check or oracle row for that pair; each refused reading increments `stellarindex_chainlink_feed_decimals_mismatch_total{consumer,pair}`. Both decimals counters are pre-seeded to zero per configured feed (`NewChainlinkReference`, `NewPoller`), so absent and healthy both read as zero.

```sh
# Which consumer/pair is affected?
curl -fs http://localhost:9465/metrics | grep '^stellarindex_chainlink_feed_decimals_'
# Configured decimals (divergence.chainlink.feeds / external.chainlink.feed_map)
grep -A3 '<pair>' /etc/stellarindex.toml
# On-chain decimals() view (0x313ce567 selector)
curl -s -X POST "$CHAINLINK_RPC_URL" -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"<feed address>","data":"0x313ce567"},"latest"]}'
```

Fix: set the operator's `decimals` for the pair to the on-chain value, or omit the field to adopt `decimals()` automatically; restart the affected process. Verify the counter's rate returns to zero. Code: `internal/divergence/chainlink_decimals.go`, `internal/sources/external/chainlink/decimals.go`; metric definitions in `docs/reference/metrics/README.md`.

## stellarindex_chainlink_feed_decimals_verify_failed

Trips (`for: 30m`, `severity: ticket`):

```promql
sum by (consumer, pair) (rate(stellarindex_chainlink_feed_decimals_verify_failed_total[15m])) > 0
```

Fail OPEN: the `decimals()` RPC call itself fails, the reader keeps serving at the last known scale (configured or previously verified) and increments `stellarindex_chainlink_feed_decimals_verify_failed_total{consumer,pair}`. Pricing flows but the scale is unverified; sustained failures mean the RPC endpoint or feed is unhealthy. Diagnose with the commands under [`stellarindex_chainlink_feed_decimals_mismatch`](#stellarindex_chainlink_feed_decimals_mismatch). Fix: confirm `CHAINLINK_RPC_URL` (or the divergence/ingest-specific RPC endpoint) is reachable and not rate-limited; switch provider if degraded; verify the rate returns to zero. A newly added feed can blip while the RPC connection warms up; `for: 30m` absorbs it.

## Related

- [Alerts catalogue](../alerts-catalog.md)
