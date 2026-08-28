---
title: Runbook — aggregator-fx-snap-fallback-dominant
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_aggregator_fx_snap_fallback_dominant`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_fx_snap_fallback_dominant` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/aggregator.yml` (group `stellarindex.aggregator`, `severity: ticket`, `for: 30m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/aggregator.yml`. |
| Typical MTTR | 15 min – 2 h |
| Impact | Chained-fiat triangulation (e.g. XLM/EUR via XLM/USD × USD/EUR) is missing the bucket-end FX snap mandated by ADR-0018. **For r1's deployed config this is worse than "degraded":** all four default chains (XLM/EUR, XLM/GBP, ETH/GBP, BTC/GBP) use fiat/fiat FX legs, which have no VWAP cache key EVER — a snap miss lands as `outcome="missing_leg"` and the chain does NOT publish. The degraded-but-publishing cached-VWAP story holds only for crypto-leg chain configs. A total leg drought is owned by `stellarindex_aggregator_triangulation_chains_dry` ([aggregator-triangulation-chains-dry.md](aggregator-triangulation-chains-dry.md)). |

## Background

The X2.5 forex-factor snap rule (Task #71) requires the FX leg of any
chained-fiat triangulation to use the most recent FX-source quote
at-or-before the bucket-end timestamp, not the rolling-window VWAP.
ADR-0018 §"Forex factor handling" mandates this for across-region
consistency: every region serving the same closed bucket reads the
same stored FX row, so the chained rate is identical across regions.

**Where the FX quotes come from (BACKLOG #42, the unified read
path):** the ACTIVE feed is **`massive`** — a forex worker running
in the **API binary** (`internal/sources/external/forex`), keyed on
`MASSIVE_API_KEY`, writing hourly `rate_usd` rows to the
`fx_quotes` hypertable. `FXQuoteAtOrBefore`
(`internal/storage/timescale/trades.go`) reads `fx_quotes` FIRST
(7-day lookback) and only then falls back to `trades` — the legacy
connector path (`polygon-forex` / `exchangeratesapi` rows),
"disabled in production but kept for compatibility if re-enabled".
On r1 neither legacy connector is configured (absent from the
`[external]` block), so their `trades` queries return nothing and
their `source_events` series don't exist — do not diagnose them.

When neither table yields a quote (`ErrNoFXQuote`), the
orchestrator tries the cached-VWAP fallback and increments
`stellarindex_aggregator_fx_snap_fallback_total{leg=...}`; for
fiat/fiat legs the fallback also misses (no VWAP cache key exists)
and the triangulation ends `outcome="missing_leg"`.

## Symptoms

- The real rule expr (note the clamp):

  ```promql
  sum(rate(stellarindex_aggregator_fx_snap_fallback_total[15m]))
  /
  clamp_min(
    sum(rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])),
    1
  ) > 0.5
  ```

  for 30+ minutes.
- **Documented blind spot** (from the rule file's own comment):
  the denominator is `clamp_min(ok_rate, 1)` — with `ok` pinned at
  ZERO the ratio can never reach 0.5, so a **total** leg drought is
  exactly the state this alert cannot see.
  `stellarindex_aggregator_triangulation_chains_dry` exists to
  cover it.
- `/v1/price` chained-fiat responses stop updating for the affected
  chains (fiat/fiat legs — see Impact).

## Quick diagnosis (≤ 5 min)

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

Decision tree:

| fx_quotes state | Probable cause | Mitigation |
| --------------- | -------------- | ---------- |
| Fresh rows (< 2 h) | Snap-rule logic bug — alert is real but FX is healthy | File an issue; check recent commits to the triangulation snap path + `FXQuoteAtOrBefore` |
| Stale (2 h – 7 d) | massive worker lagging / repeated poll failures | `stellarindex_external_fx_feed_stale` should also be firing — route to [fx-feed-stale.md](fx-feed-stale.md) |
| Older than 7 d | Beyond the snap lookback — every snap misses | Same as above, but now the chains are dry; expect `chains_dry` too |
| No rows at all | massive worker never ran (fresh deploy / key missing) | Check `MASSIVE_API_KEY` in `/etc/default/stellarindex` — the worker runs in the API binary, NOT the indexer. If the chains have stopped publishing entirely, that state is [aggregator-triangulation-chains-dry.md](aggregator-triangulation-chains-dry.md) territory |

## Mitigation (≤ 15 min)

- [ ] **massive feed stale / absent**: the sibling alerts
      `stellarindex_external_fx_feed_stale` / `_absent`
      (`configs/prometheus/rules.r1/external-pollers.yml`) watch
      exactly this; follow [fx-feed-stale.md](fx-feed-stale.md) —
      typically an expired/missing `MASSIVE_API_KEY` in
      `/etc/default/stellarindex` or upstream Massive degradation.
      Restart the API service after fixing the env.
- [ ] **fx_quotes fresh but fallbacks continue**: the snap read
      itself is suspect — check the API↔aggregator config agree on
      FX sources (`external.FXSources()`), and recent changes to
      `FXQuoteAtOrBefore` / the triangulate snap path.
- [ ] **Chains stopped publishing** (fiat/fiat legs, r1's config):
      treat as the `chains_dry` incident even if that alert hasn't
      crossed its `for:` yet — the customer-visible symptom is
      missing chained-fiat prices, not drift.
- [ ] **Verification**: fallback rate returns to near-zero within 30m
      of restoring the FX feed.

## Root cause analysis

Capture for the postmortem:

- A 1-hour metric range showing the fallback spike + recovery.
- `external_fx_last_quote_unix` + API-binary forex-worker logs
  across the incident window.
- `fx_quotes` row activity for the affected tickers across the same
  window — when did rows plateau, when did they resume?
- Whether the affected chains actually stopped publishing
  (`outcome="missing_leg"` rate) — for fiat/fiat legs that is the
  expected consequence, and it's the customer-facing half of the
  incident.

## Known false-positive patterns

- **First 30 min after a fresh deploy**: the massive worker needs
  its first successful poll before `fx_quotes` has rows for early
  buckets. Usually absorbed by the 30-min `for:` clause.
- **Aggregator restart followed by API restart**: brief flurry
  of fallbacks while the worker re-polls; usually clears in
  ≤ 5 min and never reaches the 30-min `for:` clause.
- **Bucket-end timestamp at exactly the latest FX row's `ts`**: the
  query is `<=`, so this hits — but if a region's clock is ahead of
  the FX source's publish time by even one second, the cutoff goes
  past the latest row and the next bucket's snap misses. Cross-region
  clock skew of >1 s is its own alert (chrony/timesyncd); investigate
  there if this pattern is observed.

## Related

- ADR-0018 §"Forex factor handling" — why the snap rule exists.
- `aggregator-triangulation-chains-dry.md` — the total-drought
  sibling this alert is structurally blind to (clamp_min floor).
- `fx-feed-stale.md` — the massive feed's own staleness/absence
  alerts (`rules.r1/external-pollers.yml`).
- `aggregator-silent.md` — fires when the aggregator produces zero
  writes across the board (full outage, not just FX).
- `internal/storage/timescale/trades.go::FXQuoteAtOrBefore` — the
  unified FX read path (fx_quotes first, trades legacy fallback).
- `internal/sources/external/forex/` — the massive worker.

## Changelog

- 2026-08-29 — re-verified against HEAD. Diagnosis retargeted from
  the retired connector path: polygon-forex / exchangeratesapi are
  disabled in production (absent from r1's `[external]` block) — the
  old trades queries return nothing and their `source_events` series
  don't exist. The ACTIVE feed is `massive` (forex worker in the API
  binary, `MASSIVE_API_KEY`, hourly rate_usd rows into `fx_quotes`;
  `FXQuoteAtOrBefore` reads fx_quotes first with a 7-day lookback,
  trades is the disabled-in-production fallback). Impact corrected:
  r1's four chains use fiat/fiat FX legs with no VWAP cache key, so
  a snap miss = `missing_leg` and the chain does NOT publish —
  degraded-but-publishing holds only for crypto-leg configs; the
  total-drought case rerouted to `chains_dry`. Symptom expr replaced
  with the real clamp_min form + its documented ok-pinned-at-zero
  blind spot. Rule citation → `rules.r1/aggregator.yml`; commands
  use r1 shapes; fx-feed-stale + chains_dry siblings added.
- 2026-05-01 — initial draft alongside the X2.5 snap-rule
  implementation (Task #71).
