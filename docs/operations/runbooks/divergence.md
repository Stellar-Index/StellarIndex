---
title: Runbook — divergence and oracle alerts
last_verified: 2026-10-05
status: draft
---

# Runbook — divergence and oracle staleness

## At a glance

Oracle and reference divergence alerts; each section below is one alert.

Rules: `configs/prometheus/rules.r1/divergence.yml` (group `stellarindex.divergence`; twin `deploy/monitoring/rules/divergence.yml`). All alerts are `severity: ticket` except the 5% tier (`informational`). Aggregate prices keep serving; only the divergence flag, `/v1/oracle/*` or a Chainlink feed is degraded. Aggregator metrics `localhost:9465`, indexer `localhost:9464`; r1 is `ssh root@136.243.90.96`.

## Reading the divergence flag

`flags.divergence_warning` on `/v1/price` is true only when at least `min_sources_for_warning` references answered (default 2), the references' median differs from our price by more than `threshold_pct` (default 5) or none agrees with us, and that persisted for `WarningPersistence` (default 5 min). It is cached per pair at Redis key `div:<base>/<quote>`:

```sh
redis-cli GET 'div:<base>/<quote>'
```

Read `warning_fired`, `success_count`, `agreement_count`, `divergence_pct`. Detail: `GET /v1/divergence` and `GET /v1/divergence/series`. Which references are live: the aggregator startup line `divergence refresher wired`. Oracle references (Reflector, Redstone, Band) read our own `oracle_updates` rows, so a frozen one is an ingestion question ([oracle_stale](#stellarindex_oracle_stale)). Passes run per pair at most every `[aggregate] divergence_min_interval_seconds` (default 300 s); outcomes land on `stellarindex_divergence_refresh_total{outcome}`.

## stellarindex_price_divergence_warning

`stellarindex_divergence_max_abs_fraction > 0.05`, `for: 10m`, informational. The gauge is the worst `|ours - ref| / ref` per `{reference}`; `stellarindex_divergence_pairs_over{threshold="5pct"|"10pct"}` counts pairs and `divergence_observations` names them. Over 100x is always a decoder bug, never a market move.

### Check

```sh
psql -d stellarindex -c \
  "SELECT asset_id, quote_id, reference, our_price, ref_price, delta_pct, observed_at
   FROM divergence_observations
   WHERE status = 'firing'
     AND observed_at > now() - interval '15 minutes'
   ORDER BY abs(delta_pct) DESC LIMIT 10;"
curl -s 'http://localhost:3000/v1/price?asset=native&quote=fiat:USD'
curl -s 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'
psql -d stellarindex -c \
  "SELECT source, count(*) AS trades, max(ts) AS latest
   FROM trades
   WHERE (base_asset IN ('native', 'crypto:XLM')
          OR quote_asset IN ('native', 'crypto:XLM'))
     AND ts > now() - interval '1 hour'
   GROUP BY source ORDER BY trades DESC;"
```

### Likely causes

- Stale source in the aggregate: one source's `max(ts)` far behind and off-market. A stopped source keeps contributing for the aggregation window. See [`ingestion.md#stellarindex_ingestion_source_stopped`](ingestion.md#stellarindex_ingestion_source_stopped).
- Wrong decimals (ADR-0003 `int64(parts.Lo)` truncation): ratio off by 2^N or ~0 on one source. Data-integrity incident.
- A broken reference (CoinGecko 429, frozen Reflector/Redstone/Band/Chainlink): every asset diverges from that one reference. If all references are dark see [no_reference](#stellarindex_divergence_no_reference).
- Illiquid market or CEX halt: real divergence, not an incident.

### Fix

1. Eyeball the order of magnitude, find the contributing sources and any stale one.
2. Decoder bug: revert the decoder PR, audit and delete the trades ingested with that version from `trades`, redeploy (postmortem).
3. Stale source: mitigate per the ingestion runbook; disable a chronically stopped one.
4. Broken reference: set it `enabled = false` under `[divergence]` and restart aggregator and API.
5. Verify divergence is under 5% (10% for critical) and the alert clears.

Escalate: any decoder-bug finding.

## stellarindex_price_divergence_critical

`stellarindex_divergence_max_abs_fraction > 0.10`, `for: 10m`, ticket. Same gauge, check and fix as [warning](#stellarindex_price_divergence_warning); over 10% is usually a bad ingest (wrong decimals, stale source), so go straight to the first two causes.

## stellarindex_oracle_stream_rows_unparsed

`for: 10m`, ticket. Expr:

```promql
increase(stellarindex_oracle_stream_rows_unparsed_total[6h]) > 0
or (
  stellarindex_oracle_stream_rows_unparsed_total > 0
  unless stellarindex_oracle_stream_rows_unparsed_total offset 6h
)
```

`LatestOracleStreams` could not parse the stored `asset`/`quote` text on some `<source>` rows, so they are absent from `/v1/oracle/streams` and the explorer `/oracles` page. Counter `{source,field}` is deliberately not pre-seeded.

### Check

```sql
SELECT DISTINCT source, asset, quote
  FROM oracle_updates
 WHERE source = '<source>'
 ORDER BY 1,2,3;
```

Compare with the forms `internal/canonical` accepts: `native`; classic `USDC-GA5ZSEJ...` (code, dash, 56-char G-strkey); Soroban `CA...` (56-char C-strkey); `fiat:USD`; `crypto:BTC`; `raw:<symbol>`.

### Likely causes

- Raw SQL `UPDATE` relabelling a row with a typo (the column has no `CHECK`): truncated or lower-cased strkey, missing prefix, stray space, `rwa:`/`raw:` confusion.
- A canonical form changed under the running binary.

### Fix

1. Correct the row with a parameterised update.
2. Re-read `/v1/oracle/streams` and confirm the row is back (the alert clearing only means no new drops).
3. Text correct but rejected: compare the deployed version with `internal/canonical` before editing data.

Related: [`ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols`](ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols).

## stellarindex_oracle_stale

`for: 2m`, ticket. Expr (keep it bare; do not add `on()`/`ignoring()`):

```promql
(time() - stellarindex_oracle_last_update_unix)
  > stellarindex_oracle_staleness_budget_seconds
```

Both gauges carry `{source, asset}`. Budget default is 10 x the source's declared resolution (`internal/obs/oracle_staleness.go`). `source` is an on-chain oracle (`reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`, `band`) or a poller (`chainlink`, `coingecko`, `coinmarketcap`, `cryptocompare`, `ecb`, `exchangeratesapi`); for pollers read `stellarindex_external_poller_polls_total{source=...}` and `stellarindex_external_poller_last_success_unix`.

### Check

```sh
curl -s http://localhost:9464/metrics |
  grep -E "stellarindex_oracle_last_update_unix|stellarindex_oracle_staleness_budget_seconds|stellarindex_oracle_resolution_seconds"
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
```

Contract activity: `https://stellar.expert/explorer/public/contract/<contract-id>` (r1 runs no stellar-rpc). Missing budget gauge for every asset means the indexer predates it: restart onto the current binary. Rule out a budget that never matched real cadence first:

```sh
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=604800 / changes(stellarindex_oracle_last_update_unix{source="<source>"}[7d])'
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_oracle_staleness_budget_seconds{source="<source>"}'
curl -s --get http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_oracle_resolution_seconds{source="<source>"}'
```

### Likely causes

- On-chain fresh but zero in our metrics: indexer/dispatcher stalled or decoder rejecting events (`stellarindex_source_decode_errors_total`, [`ingestion.md#stellarindex_ingestion_decode_error`](ingestion.md#stellarindex_ingestion_decode_error)).
- Publisher down (Reflector relayer, Redstone DataService, Band bot): affected prices carry `stale=true`.
- One asset stops while siblings flow: it publishes on its own rhythm or was de-listed.
- Budget too tight for a slow asset (the main false positive).

### Fix

1. Indexer stalled: `ssh root@136.243.90.96 systemctl restart stellarindex-indexer` (resumes from the persisted cursor).
2. Every asset's measured interval exceeds its budget: fix the source constant (`DefaultResolutionSeconds` in `internal/sources/<oracle>/events.go`, or `OracleResolution` in `internal/sources/external/registry.go`).
3. One slow asset: give it its own budget in `/etc/stellarindex.toml` (codified in `configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`). `asset` must be the exact canonical label; the indexer refuses to start on a bare or aliased spelling. Size it from the worst measured gap plus headroom and state it in `reason`:

```toml
[[oracle.staleness_overrides]]
source         = "reflector-cex"
asset          = "crypto:DAI"     # canonical form, NOT "DAI"
budget_seconds = 32400
reason         = "peg asset, republished only on movement; measured gaps to 7h"
```

4. Verify `stellarindex_oracle_last_update_unix` advances.

Escalate: multiple oracle sources stale at once is P1 (every oracle triangulation path is broken).

## stellarindex_divergence_refresh_error_dominant

`for: 30m`, ticket. Expr:

```promql
sum(rate(stellarindex_divergence_refresh_total{outcome="refresh_error"}[5m]))
  >
sum(rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m]))
and
sum(rate(stellarindex_divergence_refresh_total{outcome=~"refresh_error|ok"}[5m])) > 0
```

The refresher runs but something downstream fails, so the flag stops updating and shows no warning after the cache TTL (>= 5 min) lapses even when prices diverge. Log: `divergence refresh failed`.

### Check

```sh
curl -fs http://localhost:9465/metrics | grep '^stellarindex_divergence_refresh_total'
journalctl -u stellarindex-aggregator --no-pager | grep 'divergence refresher wired' | tail -1
journalctl -u stellarindex-aggregator -n 200 | grep 'divergence refresh failed'
curl -fs 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'
```

### Likely causes

- HTTP 429 from CoinGecko: anonymous tier, one batched call per pass; the price reference has no API-key field.
- HTTP 5xx from CoinGecko: self-recovers in 5-30 min.
- Redis cache write failed: [`cache.md#stellarindex_redis_writes_blocked`](cache.md#stellarindex_redis_writes_blocked).
- All references failed: egress firewall or DNS on the aggregator host.

### Fix

1. Identify the failing reference from the logs and the `wired` line; probe it by hand.
2. If recovery is slow, set its `enabled = false` under `[divergence]` and restart the aggregator (the zero-quota oracle references keep feeding).
3. Verify `rate(stellarindex_divergence_refresh_total{outcome="refresh_error"}[5m])` falls below `ok`.

## stellarindex_divergence_no_reference

`for: 30m`, ticket. Expr:

```promql
sum(rate(stellarindex_divergence_refresh_total{outcome="no_reference"}[5m]))
  >
sum(rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m]))
and
sum(rate(stellarindex_divergence_refresh_total{outcome="no_reference"}[5m])) > 0
```

Every configured reference is dark for the pair (`ErrNoReferenceResponded`): `flags.divergence_warning` freezes at its last value and `divergence_checked` reads false; a firing warning is neither cleared nor re-sent.

### Check

```sh
curl -fs http://localhost:9465/metrics | grep '^stellarindex_divergence_refresh_total'
journalctl -u stellarindex-aggregator | grep 'divergence refresher wired' | tail -1
curl -fs 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'
```

### Likely causes

- CoinGecko free-tier 429 with no other reference covering the pair (no key-based fix).
- Every on-chain oracle feed for the pair stale (see the staleness sections above).
- Egress blocked: every reference dark at once.
- Known-uncovered pair, or a quote older than `divergence.MaxComparableAge` (1 h): not an outage.

### Fix

1. Restore the failing reference: wait out the 429 or add a non-CoinGecko reference for the pair.
2. Restart the aggregator after any config change. Any one responding reference clears the alert.
3. Verify `rate(stellarindex_divergence_refresh_total{outcome="no_reference"}[5m])` falls below `ok`.

## stellarindex_divergence_no_ok_outcomes

`for: 15m`, ticket. Expr (gated on the refresher being wired):

```promql
sum(increase(stellarindex_divergence_refresh_total{outcome="ok"}[30m])) == 0
and on()
max(stellarindex_divergence_refresher_wired) == 1
```

The pass produces no `ok` and neither failure rule sees it (only `no_vwap`, or nothing). No `div:<asset>` entry written for 45+ min.

### Check

```sh
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_divergence_(refresh_total|refresher_wired)'
curl -fs http://localhost:9465/metrics | grep '^stellarindex_anomaly_freeze_active'
journalctl -u stellarindex-aggregator --since '-1h' | grep -E 'divergence refresh (panicked|: no vwap)' | tail
```

### Likely causes

- Every pair frozen (ADR-0019): `no_vwap` climbs. Follow [`anomaly.md#stellarindex_anomaly_freeze_engaged`](anomaly.md#stellarindex_anomaly_freeze_engaged).
- No VWAP in cache without a freeze: `min_usd_volume` above real volume, or the VWAP writer regressed.
- Pass panics before counting (`divergence refresh panicked`).
- False positive: `divergence_min_interval_seconds` above 30 min.

### Fix

1. Frozen: resolve the freeze; divergence recovers once a fresh VWAP is cached.
2. No VWAP: restore the `[aggregate]` volume floor or fix the writer; confirm `stellarindex_aggregator_vwap_writes_total` climbs.
3. Panic: capture the stack, restart the aggregator.

## stellarindex_divergence_reference_failing

`for: 30m`, ticket. Expr:

```promql
sum by (reference) (rate(stellarindex_divergence_reference_total{outcome!~"ok|asset_unsupported|too_stale_to_compare"}[15m]))
  >
0.5 * sum by (reference) (rate(stellarindex_divergence_reference_total{outcome!~"asset_unsupported|too_stale_to_compare"}[15m]))
```

More than half of one reference's lookups failed. The only signal for a partial outage (the pass still counts `ok` while another reference answers).

### Check

```promql
sum by (reference, outcome) (rate(stellarindex_divergence_reference_total[15m]))
```

`price_unavailable` from CoinGecko is its freshness gate failing closed; `timeout` / `overall_deadline_exceeded` is a slow upstream; `panicked` also moves `stellarindex_worker_panics_total`.

### Fix

Remediate the reference as in [no_reference](#stellarindex_divergence_no_reference).

## stellarindex_divergence_pair_below_quorum

`for: 30m`, ticket. Expr:

```promql
max by (pair) (max_over_time(stellarindex_divergence_pair_quorum_met[30m])) == 0
```

Every refresh for 60+ min had fewer responding references than `min_sources_for_warning`. The verdict is carried forward, so a live depeg cannot warn while the pass stays `ok`.

### Check

```promql
stellarindex_divergence_pair_quorum_met == 0
```

Find the dropped reference with `stellarindex_divergence_reference_total` by `outcome`.

### Fix

Remediate as for [reference_failing](#stellarindex_divergence_reference_failing). A pair whose only other coverage is a daily-heartbeat feed sits below quorum between pushes: add a fresher reference.

## Related

- [Alerts catalogue](../alerts-catalog.md)
