---
title: Runbook — supply assets stale
last_verified: 2026-07-28
status: ratified
severity: P3
---

# Runbook — `stellarindex_supply_assets_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_assets_stale` |
| Severity | **P3** (ticket) |
| Detected by | `stellarindex_supply_assets_stale > 0` for > 2h |
| Emitted by | `data-freshness.sh` → node_exporter textfile (`data_freshness.prom`), every 15 min |
| Typical MTTR | 30 min–hours (usually a freshness-gate or observer problem, not a dead DB) |
| Impact | The named count of watched assets are serving a FROZEN supply figure — and therefore a frozen market cap / FDV. The API keeps answering; the numbers are just old, which is worse than an obvious outage because nothing looks broken. |

## Why this alert exists separately from `data_source_stale`

`stellarindex_data_freshness_stale{domain="supply"}` measures
`max(time)` across the WHOLE `asset_supply_history` table. It proves only
that **some** asset is publishing.

On 2026-07-28 that read green while **37 of 48 watched assets were frozen**,
some for over two weeks: the handful of still-live assets kept the global max
current and hid every other one. An aggregate cannot see a partial freeze.
This alert is the per-asset shape that aggregate cannot express.

## Symptoms

- `stellarindex_supply_assets_stale = N` (N assets with no snapshot in >30h).
- `stellarindex_supply_asset_max_age_seconds` shows the worst offender's age.
- Usually NO accompanying `data_source_stale{domain="supply"}` — that is the
  whole point of this alert.

## Triage

**1. Confirm scope — which assets, and how stale?**

```sql
SELECT asset_key, now()-max(time) AS age
FROM asset_supply_history
GROUP BY asset_key ORDER BY age DESC LIMIT 20;
```

**2. Is the refresher rejecting, or not running at all?** Read the outcome
counters — deltas, not since-boot totals (a cumulative counter read as a rate
produced a false "0% success" claim once):

```sh
curl -s http://localhost:9465/metrics | grep supply_refresh_duration_seconds_count
```

- Dominated by `stale_component` → the freshness GATE is refusing. Go to 3.
- Dominated by `no_ledger` / `compute_error` → a reader or the lake. Different
  problem; see the supply-refresh runbooks.
- `ok` / `dormant` advancing → the refresher is fine and the rows ARE landing;
  re-check the query in step 1.

**3. If the gate is refusing, compare PRODUCER progress against the asset's own
last activity.** This is the CS-102 shape and the most likely cause:

```sql
-- producer watermarks (should track tip)
SELECT max(ledger) FROM trustline_observations;
SELECT max(ledger) FROM claimable_observations;
SELECT max(ledger) FROM sac_balance_observations;
SELECT max(ledger) FROM lp_reserve_observations;
SELECT max(ledger) FROM sep41_supply_events;
SELECT max(ledger) FROM account_observations;
```

If the producer watermarks track tip but the affected assets are quiet in one
component, the data is FINE and a freshness gate is misreading quiet as stale.
**A quiet asset is not a stale asset.** Do not "fix" this by loosening the
dormancy horizon — that hides the defect and re-publishes unverified figures.

Scope any per-contract/per-asset probe with an indexed predicate. An
unbounded `GROUP BY` over `sep41_supply_events` ran 11 minutes with no output;
the index-bounded per-contract form answered in 96 ms.

**4. If a producer watermark is genuinely behind**, that is a real stall — the
gate is doing its job. Find the stalled writer (indexer/projector), and check
whether a projector tail rebuild is pending.

## Resolution

- **Gate misreading quiet as stale** → the anchor must be the producer's
  watermark, not per-entity last activity. All three supply algorithms were
  fixed this way in CS-102 (`e21fa3d0` classic, `3f26b8db` SEP-41, `aa0d08c2`
  XLM). A regression here most likely means a new supply path was added with
  the old per-entity shape.
- **Genuinely stalled producer** → restart/repair the writer, then run the
  relevant catch-up (`projector-replay` for projected sources).
- **Verify recovery** with `scripts/ops/reconcile-supply-vs-horizon.sh`, which
  checks every classic asset against Horizon's FULL component sum. Confirm the
  count returns to 0 rather than assuming the fix worked.

## Related

- [`supply-refresh-error-dominant.md`](supply-refresh-error-dominant.md)
- [`data-source-stale.md`](data-source-stale.md)
- `docs/operations/v1-launch-plan.md` — CS-102 loop entries (2026-07-28)
