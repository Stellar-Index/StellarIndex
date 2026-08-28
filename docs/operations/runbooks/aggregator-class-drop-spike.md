---
title: Runbook — aggregator-class-drop-spike
last_verified: 2026-08-28
status: current
severity: P3
---

# Runbook — `stellarindex_aggregator_class_drop_spike`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_class_drop_spike` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/aggregator.yml` (group `stellarindex.aggregator`, `severity: ticket`, `for: 15m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/aggregator.yml`. |
| Typical MTTR | 30 min |
| Impact | The class filter is dropping trades at >10× baseline. Most often this means a new venue is emitting trades that aren't yet registered in `external.Registry`, so they hit the fail-closed `IncludeInVWAP=false` fallback and don't contribute to VWAP. Ingest still records the rows; only aggregation is affected. |

## Symptoms

- `sum(rate(stellarindex_aggregator_dropped_trades_total{reason="class"}[10m]))` > 10× baseline,
  sustained for `for: 15m` before the alert fires.
- New entries in `stellarindex_source_events_total` for source labels
  the registry doesn't list.

## Quick diagnosis (≤ 5 min)

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

The rows-vs-registry diff is the smoking gun: any source name in
the trades table that doesn't appear in `internal/sources/external/registry.go`
is being treated as `Class=Exchange + IncludeInVWAP=false` (the
fail-closed fallback) and silently dropped from VWAP. Since
2026-08-14 the counter's `pair` label names the configured target
pair the drop happened under, so the metrics dump also tells you
which pair's VWAP the unregistered source was trying to feed.

## Mitigation (≤ 15 min)

- [ ] **Net-new venue intentionally being added** (someone is
      onboarding a connector): add a one-line entry to
      `external.Registry` under the appropriate `Class*` constant.
      A venue with a classifying ADR open and trade flow already in
      Timescale should be `ClassExchange + IncludeInVWAP=true`. A
      paid-tier aggregator/oracle stays `IncludeInVWAP=false`.
- [ ] **Unintentional source name** (typo, renamed connector, dev
      build leaking into prod): identify the rogue process and roll
      back. The trades it inserted are valid data — leave them in
      Timescale; they'll show up in `/v1/sources` once the source
      is registered.
- [ ] **Existing aggregator/oracle source spiked in volume**: this
      is expected — aggregator-class sources are *meant* to be
      dropped from VWAP. If the spike has a known cause (CoinGecko
      added new pairs, a Reflector contract upgraded), document
      it in the on-call channel and silence the alert for the
      relevant interval. No code change needed.
- [ ] **Verification**: `dropped_trades_total{reason="class"}` rate
      returns to baseline (or, after a registry update, the same
      rate but with the source now contributing as `IncludeInVWAP=true`
      → `vwap_writes_total` should also tick up).

## Root cause analysis

Capture for the postmortem:

- The source-label list before/after the change.
- The PR / commit that added or renamed the source.
- Confirmation that the source's class assignment is intentional.

## Known false-positive patterns

- **First hour after the alert rule lands**: this alert's own
  `offset 1h` baseline comparator misfires during the first hour
  after the rule lands (no baseline exists yet). Suppress on first
  rollout. (`aggregator-outlier-storm` no longer shares this shape —
  its 2026-08-28 redesign reads per-venue VWAP dispersion via
  `stellarindex_aggregator_venue_vwap`.)
- **Test fixtures leaking into prod metrics namespace**: a
  development binary scraped by prod Prometheus produces this
  exact symptom. Check `stellarindex_source_enabled` for sources
  that should be off in this region.

## Related

- `aggregator-outlier-storm.md` — sibling per-reason drop alert.
- `internal/sources/external/registry.go` — single-source-of-truth
  for class assignments. Adding a venue is a one-line amendment.
- ADR (TBD) — operator-facing per-source weighting once the
  registry outgrows a hand-curated map.

## Changelog

- 2026-08-28 — re-verified against HEAD. Rule citation →
  `rules.r1/aggregator.yml` with the `for: 15m` hold now stated;
  diagnosis annotated with the :9464-indexer / :9465-aggregator port
  split; psql → r1 shape; noted the counter's `pair` label
  (post-2026-08-14); false-positive section untangled from
  `aggregator-outlier-storm`, whose 2026-08-28 redesign no longer
  shares the `offset 1h` shape.
- 2026-04-25 — initial draft alongside the aggregator metrics
  PR #26 wire-up.
