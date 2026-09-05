---
title: Runbook — sla-probe-freshness-breach
last_verified: 2026-09-05
status: current
severity: P2
---

# Runbook — `stellarindex_sla_probe_freshness_breach`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_sla_probe_freshness_breach` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/sla-probe.yml` (group `stellarindex.sla_probe`, `severity: page`, `for: 30m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/sla-probe.yml` (same expr/for/labels). |
| Typical MTTR | 30–90 min |
| Impact | Detail-page consumers see stale prices. The 30 s SLA target is the DEFAULT and applies to every freshness-bearing endpoint; `/v1/price` is the one exemption, because it is closed-bucket-served (ADR-0015) and its `observed_at` is *structurally* 30–150 s old by design — the probe holds it to a 150 s verdict bound (`defaultClosedBucketFreshTarget` in `cmd/stellarindex-sla-probe/main.go`) and the alert pages at 180 s. A sustained breach means the affected price surface is out of date beyond even those allowances. |

## Symptoms

- The real expression (identical in both rule trees):

  ```promql
  stellarindex_sla_probe_freshness_sec{endpoint="price"} > 180
  or
  stellarindex_sla_probe_freshness_sec{endpoint!="price"} > 30
  ```

  sustained `for: 30m`. The 30 s SLA is the default; `/v1/price` is
  the single exemption, because it serves the last CLOSED bucket
  (ADR-0015) and its `observed_at` is structurally 30–150 s behind
  wall-clock even when everything is healthy — the probe's verdict
  bound for it is 150 s and the alert line is 180 s. Read the
  alert's `summary` for the value; the description covers both
  bounds and cannot tell you which one tripped.
- The probe's JSON report shows `observed_at` on `/v1/price` (or
  `/v1/price/tip`) beyond its per-endpoint bound. Only `price` and
  `price-tip` publish freshness today; the third probe endpoint is
  named `oracle-latest` (its label on the wire — not
  `oracle_latest`) and emits no `observed_at`, so no series exists
  for it. If it ever grows one it is held to the 30 s SLA, not to
  `/v1/price`'s exemption.
- `flags.stale: true` will be set on responses for the affected
  pair — clients gating on this flag are likely backing off, but
  many clients don't gate and surface the stale value as-is.

## Quick diagnosis (≤ 5 min)

The freshness chain has three stages — bisect by which one's lagging:

```sh
# 1. Get the probe's most recent report. The wrapper
#    (configs/healthchecks/sla-probe.sh) POSTs the JSON body to
#    Healthchecks.io ONLY — it never writes the report to journald —
#    so read it from the check's "last ping body" on the
#    Healthchecks.io dashboard, or run a one-off probe:
ssh root@136.243.90.96 '/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 -pair native,fiat:USD -report-format json' \
  | jq '.per_endpoint[] | select(.endpoint=="price")'

#    Or read the last run's exported metrics from the textfile:
ssh root@136.243.90.96 'grep freshness /var/lib/node_exporter/textfile_collector/sla_probe.prom'

# 2. Direct API check on the same pair.
curl -s 'https://api.stellarindex.io/v1/price?asset=native&quote=fiat:USD' | jq

# 3. What does Redis say for the cache key?
# F-1309: aggregator writes `vwap:<base>:<quote>:<window-seconds>`,
# not a single `price:<base>:<quote>` key. Check each window
# explicitly — TTLs differ (300s = 5m bucket, 3600s = 1h, 86400s = 1d).
ssh root@136.243.90.96 "redis-cli GET 'vwap:native:fiat:USD:300'"
ssh root@136.243.90.96 "redis-cli GET 'vwap:native:fiat:USD:3600'"
ssh root@136.243.90.96 "redis-cli GET 'vwap:native:fiat:USD:86400'"

# 4. What does Postgres say is the latest closed bucket?
# F-1309: timestamp column is `bucket`, NOT `ts` — that's the
# trades-table column.
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "
  SELECT bucket, vwap FROM prices_1m
  WHERE base_asset='"'"'native'"'"' AND quote_asset='"'"'fiat:USD'"'"'
  ORDER BY bucket DESC LIMIT 5;"'
```

If Postgres has fresh buckets but Redis is stale → aggregator
isn't writing the cache. If Postgres is also stale → ingestion
side. If both are fresh but the API returns stale → API is
reading the wrong key.

## Typical root causes (roughly in frequency order)

1. **Aggregator orchestrator down or wedged.** No one is writing
   the `vwap:<base>:<quote>:<window>` cache keys, so `/v1/price`
   falls back to the Postgres read which has the right value but
   slower path; freshness lags as the aggregator's tick gap grows.
   - Signal: `rate(stellarindex_aggregator_ticks_total[5m]) == 0`
     (the [aggregator-silent](aggregator-silent.md) alert fires
     on this directly via `stellarindex_aggregator_vwap_writes_total`).
   - Mitigation: restart the aggregator binary; investigate why
     it stopped.

2. **Indexer lag**. The dispatcher is behind on LCM consumption,
   so even fresh ledger data isn't producing closed buckets.
   - Signal: the [source-stopped](source-stopped.md) alert
     (`stellarindex_ingestion_source_stopped`) fires when a source
     shows `sum by (source) (rate(stellarindex_source_events_total[30m])) == 0`
     sustained `for: 15m` — gated on
     `stellarindex_source_enabled == 1` AND on the
     continuous-source allowlist baked into the rule (binance,
     bitstamp, coinbase, kraken, sdex, aquarius, reflector-dex/cex/fx,
     redstone, coingecko). Sporadic sources (band, blend, comet,
     ecb, phoenix, …) are deliberately excluded and won't page.
   - Mitigation: see `core-lag.md`.

3. **CAGG refresh policy is paused or lagging.** The `prices_1m`
   CAGG isn't materializing recent buckets even though raw trades
   are present.
   - Signal: `stellarindex_timescale_cagg_stale` fires too.
   - Mitigation: see `cagg-stale.md`.

4. **No trades for the pair in the tip's escalation window — the
   most common cause on `price-tip`, and it is in-contract.**
   `computeTip` tries the caller's window (5 s), escalates to 30 s,
   and only then falls back to `PriceReader.LatestPrice` — the
   CLOSED bucket. So a pair with no trade for 30+ s serves a
   60–120 s `observed_at` on the tip surface, exactly as ADR-0018
   describes, and the probe correctly records it as over the 30 s
   SLA. The tell is `price` and `price-tip` reporting the SAME
   freshness to three decimals: both endpoints are serving one
   closed bucket. Observed on r1 2026-09-05 07:19 UTC — both read
   `109.156` because XLM/`fiat:USD` had a 67-second CEX trade gap
   (07:18:31 → 07:19:38) covering two thirds of the 30-second probe
   run.
   - Signal: bounded trade query for the pair over the run window:

     ```sh
     ssh root@136.243.90.96 "cd /tmp && runuser -u postgres -- psql -d stellarindex -X -c \"
       SELECT ts, source, quote_asset FROM trades
       WHERE ts >= now() - interval '5 minutes'
         AND base_asset IN ('native','crypto:XLM')
         AND quote_asset = 'fiat:USD'
       ORDER BY ts;\""
     ```

   - Mitigation: none on the serving side — the closed bucket IS the
     honest answer. If a pair is chronically this quiet the fix is
     source coverage, not a threshold. Ack, and do not widen the
     30 s bound: it is the published SLA.

5. **No trades for the asset at all.** Legitimate market quiet —
   the asset just hasn't traded. The "stale" flag is correct, not a
   bug.
   - Signal: trade-count panel for the pair shows zero recent rows.
   - Mitigation: this is expected; mark the alert as ack'd if the
     asset is known-thin.

## Mitigation

- [ ] Step 1 — Bisect via Quick diagnosis to identify the lagging
      stage.
- [ ] Step 2 — Route to the stage-specific runbook
      (aggregator / indexer / CAGG).
- [ ] Step 3 — If "no trades in window" — this is honest staleness.
      Confirm the pair is genuinely quiet and ack the alert.
- [ ] Verification: probe `freshness_sec` back under the
      per-endpoint bound (180 s for `price`, 30 s for everything
      else) for ≥ 30 min.

## Known false-positive patterns

- **Measurement bias from the run duration — fixed 2026-09-05, but
  check the binary version first.** The probe used to compute
  freshness as `time.Since(observed_at)` during aggregation, which
  runs once AFTER the whole run, so every sample was charged the
  distance from its own request to the end of the run: a median
  bias of `SLA_PROBE_DURATION / 2`. On r1 at the 30 s default the
  tip read ~15 s against a sub-second truth; at the 120 s duration
  `configs/healthchecks/sla-probe.sh` recommends for
  memory-pressured hosts it would have read ~60 s and paged
  permanently. Freshness is now anchored to each sample's own
  receipt instant (`probeSample.receivedAt`). If the deployed
  binary predates that fix, subtract `run_duration_seconds / 2`
  from the reading before believing it.
- **Newly-listed asset** with low trading volume. Freshness can
  easily exceed the target if no one's trading the pair. Consider
  adding the asset to a "thin-pair allowlist" if this pattern is
  expected to persist.
- **Maintenance windows** — if the indexer or aggregator is
  intentionally paused for migration, the probe will fire.
  Pre-silence the alert before the maintenance window.

## Related

- `cagg-stale.md` — Postgres-side staleness.
- `core-lag.md` — indexer-side lag.
- `aggregator-silent.md` — orchestrator not writing.
- The service freshness SLA — the 30 s spec (tip-of-chain surface).
- ADR-0015 — the closed-bucket-only serving contract that makes
  `/v1/price` structurally 30–150 s old.

## Changelog

- 2026-09-05 — freshness selector inverted to a positive matcher:
  `{endpoint="price"} > 180 or {endpoint!="price"} > 30`. The old
  `{endpoint!="price-tip"} > 180` arm handed `/v1/price`'s ADR-0015
  closed-bucket exemption to every endpoint that merely was not the
  tip, so the first other endpoint to publish an `observed_at`
  would have inherited a bound six times the SLA. Added the
  `computeTip` 30 s-escalation fallback as root cause 4 (the
  identical-value-across-two-endpoints tell) and the pre-fix
  run-duration measurement bias as a false-positive pattern.
- 2026-08-29 — re-verified against HEAD (Wave I). The runbook
  claimed a universal 30 s target: real expr is two-armed —
  `price-tip > 30` OR every other endpoint `> 180` (`for: 30m`),
  because `/v1/price` is closed-bucket-served (ADR-0015,
  structurally 30–150 s old; probe verdict bound
  `defaultClosedBucketFreshTarget` = 150 s). The
  `journalctl | jq` retrieval step could never work — the wrapper
  POSTs the JSON report to Healthchecks.io only, never journald;
  replaced with the dashboard body / one-off probe / textfile
  reads. Endpoint name corrected to `oracle-latest`; commands
  moved to r1 shapes; source-stopped description replaced with the
  real gated + allowlisted rule. Status promoted ratified → current.
- 2026-04-30 — initial draft alongside #294 (alert rules).
