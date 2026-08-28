---
title: Runbook — price-stale
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_api_price_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_price_stale` |
| Severity | P2 (ticket) |
| Detected by | `configs/prometheus/rules.r1/api.yml` (r1 overlay — source of truth for r1); multi-host mirror in `deploy/monitoring/rules/api.yml` |
| Typical MTTR | 15–60 min |
| Impact | `/v1/price?asset=<X>` returns a price but `observed_at` is stale. Clients get a real answer; it's just old. Envelope `stale=true` flag is set whenever the fallback chain (last-trade / stablecoin proxy / triangulation) served the answer, but the gauge captures the underlying staleness even on the happy path. |

## Symptoms

- `stellarindex_price_staleness_seconds{asset=...} > 120` sustained
  5 min. `asset` is one of the aggregator's *configured pair bases*
  (`native`, `crypto:XLM`, `crypto:BTC`, `crypto:ETH` — the
  `defaultPairs` list in `cmd/stellarindex-aggregator/main.go`), not
  the `CODE-ISSUER` form a customer passes to `/v1/price`.
- **OR** the series has been ABSENT for 10 min
  (`absent_over_time(stellarindex_price_staleness_seconds[10m])`,
  F-0104). In that case the alert fires with **no `asset` label**:
  the aggregator tick is wedged and has stopped emitting the gauge.
  Go straight to `aggregator-silent.md`.
- `/v1/price?asset=<X>` returns 200 with a price but the
  `observed_at` timestamp is well behind wall-clock.
- TODO(ash): the *Price → freshness* Grafana dashboard referenced by
  earlier revisions of this runbook has no JSON in the repo; link the
  live dashboard UID here or drop the reference.

## Quick diagnosis (≤ 5 min)

```sh
# Which branch fired? No `asset` label ⇒ absent branch ⇒ aggregator-silent.md
curl -s http://localhost:9090/api/v1/alerts |
  jq '.data.alerts[] | select(.labels.alertname=="stellarindex_api_price_stale") | {labels, value, activeAt}'

# Which asset is stale? (aggregator metrics endpoint, 127.0.0.1:9465)
curl -s http://127.0.0.1:9465/metrics |
  awk '/^stellarindex_price_staleness_seconds/ && $2 > 120 {print}'

# Is it one asset or many?
#   One asset → that asset's source is stopped / paused, or the pair
#     isn't clearing the VWAP publication gate (root cause 3).
#   Many assets on one source → the source is stopped.
#   Many sources → the aggregator isn't writing (or isn't running).
#   Note: `native` and `crypto:XLM` always report the SAME value
#   (the gauge emits MIN over both forms for both labels).

# Which sources quote this asset? (on-chain XLM rows are stored as
# `native`, CEX rows as `crypto:XLM`; the rc.89 dual-form alias
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

## Typical root causes

1. **Source quoting this asset is stopped.** Events stop → no new
   trade → aggregator has nothing fresh to roll up → API serves
   the last trade with an aging `observed_at`.
   - Signal: `stellarindex_source_last_event_unix{source=<X>}` is
     frozen; `stellarindex_ingestion_source_stopped` alert may also
     have fired on that source (it uses `for: 15m`, so it can lag
     this alert).
   - Also compare against `stellarindex_source_last_insert_unix`
     (both on the indexer at `127.0.0.1:9464`). Events advancing
     while inserts are frozen is the stuck-cursor / duplicate-flood
     signature — see `cursor-stuck.md` /
     `ingestion-duplicate-flood.md` and the
     `stellarindex_serving_insert_frozen` alert.
   - Mitigation: `source-stopped.md`.

2. **Aggregator is running but not writing CAGGs / hot-cache**.
   Happens when CAGG refresh jobs fail (schedule misfire, SQL
   error in the window function).
   - Signal: `stellarindex_timescale_cagg_stale` alert.
   - Mitigation: `cagg-stale.md`.

3. **Aggregator running but the pair has had no VWAP write for
   > 120 s.** The gauge is emitted by the aggregator at the end of
   *every* tick for *every* configured pair
   (`internal/aggregate/orchestrator/orchestrator.go`
   `emitStalenessGauges`, reset on each VWAP cache write). It is
   not request-driven and the alert rule uses no `change()` — a
   reading > 120 s means the pair genuinely has not been published
   in that window. Causes: the pair isn't clearing the
   `min_usd_volume` publication gate (`$10k`/window in
   `/etc/stellarindex.toml`), empty windows, outlier filtering
   dropping everything, or an anomaly freeze engaged.
   - Signal: `stellarindex_aggregator_empty_windows_total` climbing;
     freeze-lifecycle alerts (`anomaly-freeze-engaged.md`);
     `fx-feed-stale.md` for fiat legs.

4. **Pair that doesn't really trade on-chain anymore.** Classic
   long-tail assets: the last legitimate trade was days ago. This
   is a *data reality* not a bug — but it exposes clients to very
   stale prices. Note that only the configured pair bases carry
   this gauge: long-tail classic assets can **never** fire this
   alert; their staleness surfaces via the API `stale` flag and the
   sla-probe / served-value-drift alerts instead. Consider
   de-listing the asset from the API response or flagging
   `stale=true` in the envelope (we already do if fallback fired).

5. **Binary version skew.** Aggregator and API/indexer on different
   builds after a partial deploy (alert
   `stellarindex_binary_version_skew`, added 2026-08-28).
   - Signal: `-version` output of `/usr/local/bin/stellarindex-aggregator`
     vs `/usr/local/bin/stellarindex-api` differs.
   - Mitigation: `binary-version-skew.md`.

## Mitigation

- [ ] Step 0 — if the alert carries no `asset` label (absent
      branch): `aggregator-silent.md`.
- [ ] Step 1 — confirm which sources quote the asset, and which of
      them has stopped (see diagnosis above).
- [ ] Step 2 — if one source is dead: `source-stopped.md`.
- [ ] Step 3 — if the aggregator pipeline is the problem:
      `cagg-stale.md`.
- [ ] Step 4 — if there's genuinely no on-chain activity: decide
      with product whether to de-list or keep the stale number
      with `stale=true`.
- [ ] Verification: `stellarindex_price_staleness_seconds{asset=<X>}`
      drops back under 120 s and the alert clears (`for: 5m` gives
      you time to verify it's not a flap).

## Root cause analysis

- Which asset(s) were stale — was it a pattern (all tokens from
  one issuer, all pairs quoted by one source)?
- Aggregator logs covering the window — any errors, any refresh
  lag?
- Source `health()` reports from the orchestrator — was it
  throwing soft errors (decode-errors up) while still running?

## Known false-positive patterns

- **Aggregator restart**: `lastWriteAt` resets on restart, so every
  pair reports ~0 and then climbs. A freshly-configured pair is also
  stamped "just observed" on first sighting (reports ~0). If no VWAP
  write lands within 2 min of the restart the alert is real, not a
  warm-up artifact.
- **Chain halt**: if Stellar mainnet itself stops producing ledgers
  (rare), every asset goes stale simultaneously — correlates
  strongly with `core-lag.md` / `rpc-lag.md`. The price-stale alert
  in this case is redundant noise; the core/rpc alert is the real
  one.

## Related

- `aggregator-silent.md` — the absent-series branch of this alert.
- `source-stopped.md` — when a specific source has stopped dispatching.
- `cagg-stale.md` — when aggregation jobs fail to refresh.
- `oracle-stale.md` — oracle-specific staleness.
- `sla-probe-freshness-breach.md` — the customer-facing freshness
  alert (`/v1/price/tip` > 30 s, other endpoints > 180 s).
- `data-source-stale.md`, `served-value-drift.md` — adjacent
  freshness / drift signals.
- `binary-version-skew.md` — half-upgraded aggregator (root cause 5).
- HA plan §9 degradation envelope: `docs/architecture/ha-plan.md`.

## Changelog

- 2026-08-28 — re-verified against HEAD. Gauge is aggregator-emitted
  per tick (F-1306), not request-driven; documented the
  `absent_over_time` branch (F-0104), the configured-pair label set,
  the insert-side freshness signal, and version skew as a cause.
- 2026-04-23 — initial draft. Documented the (since replaced)
  request-driven per-asset gauge behaviour.
