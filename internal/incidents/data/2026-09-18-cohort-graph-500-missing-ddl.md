---
title: "[SEV-3] Account cohort graph 500s after v0.91.0 deploy — 2026-09-18"
date: 2026-09-18
severity: SEV-3
status: resolved
started_at: 2026-09-18T10:31:00Z
resolved_at: 2026-09-18T11:07:00Z
affected_components:
  - api
postmortem:
---

# [SEV-3] Account cohort graph 500s after v0.91.0 deploy

## What happened

Reconstructed after the fact: this incident was never opened live — the
only record of it was a CHANGELOG paragraph and the commit message of
`bdaecccc5`. This file backfills the SEV runbook procedure that should
have run on 2026-09-18.

The 10:31Z deploy cleared v0.91.0 to r1 even though the deploy's own
ClickHouse evidence step published a `::warning::` naming
`stellar.asset_month_usd_prices` and its staging twin as absent on the
target host. The config-apply gate at the time could not distinguish
"nobody asked whether this surface exists" from "asked, and the host
said no" — an operator's `config_acknowledged=true` cleared both alike,
so the missing-table warning did not block the release.

v0.91.0's cohort flows read (`internal/storage/clickhouse/
account_cohort_rollup.go`, `readCohortFlows`) `LEFT JOIN`s
`asset_month_usd_prices` to attach each month's then-price. ClickHouse
raises rather than returning nulls when the joined table does not
exist, so every `GET /v1/accounts/{g}/graph/cohort` request — the whole
route, not just the priced fields — answered 500 in ~0.33s (down from a
normal ~1.5s 200) until the missing DDL was applied by hand.

## Impact

- **Endpoints affected:** `GET /v1/accounts/{g}/graph/cohort` (both
  `relation=created` and `relation=sponsored`) — 100% of requests to
  this one route.
- **Customers affected:** anyone reading the explorer's account cohort
  panel or calling the endpoint directly. No other `/v1/accounts/*`
  routes were affected.
- **Data correctness:** no data served — 500, not stale or wrong data.
- **Workaround:** none; the route was unusable until remedy.

## Timeline

All times UTC.

| Time (UTC) | Update |
|-----------|--------|
| 10:31 | Deploy clears v0.91.0 to r1. The ClickHouse evidence step's own `::warning::` names `stellar.asset_month_usd_prices` and `…_staging` as absent on the host; the config-apply gate's acknowledgement path clears the release anyway. |
| 10:31–~11:00 | Every `GET /v1/accounts/{g}/graph/cohort` request answers 500. No paged alert or status-page update was raised live; the gap was found only in this reverification sweep. |
| ~11:00 | `stellar.asset_month_usd_prices` (+ staging twin) DDL applied by hand on the host, matching the operator runbook (`account_cohort_rollup.sql`). Route recovers. |
| 11:07 | `bdaecccc5` lands: the evidence step now publishes a `refuted` output beside `applied`, and the config-apply gate refuses an acknowledgement for any surface the host proved unapplied — closing the class of gap that let this deploy through. |

## What we did

Applied the missing `asset_month_usd_prices` DDL by hand to restore the
route, then landed `bdaecccc5` so a future deploy cannot clear a
surface the evidence step already proved absent on the host. Separately
(GH-1078), `internal/storage/clickhouse/account_cohort_rollup.go`'s
cohort-flows read no longer fails the whole route when that table is
absent: it falls back to serving flows without then-prices instead of
LEFT JOIN-erroring the request closed, so a future gap in this optional
enrichment table degrades rather than 500s.

## Postmortem

Not written at the time; this file is the first record. A full
postmortem was not produced for this SEV-3 and none is scheduled
retroactively — the two concrete remediations (gate fix + fail-open
read) are recorded above and in `bdaecccc5` / GH-1078.

## Lessons learned

- A deploy gate that can be cleared by an acknowledgement needs to
  distinguish "no evidence was gathered" from "evidence says no" —
  conflating them let a known-missing table ship.
- An optional enrichment table joined into a base read should degrade,
  not fail the request: the base cohort-flows rows do not depend on
  `asset_month_usd_prices` existing.
- This incident surfaced only via a documentation/backlog reverification
  sweep, not the SEV runbook. `/v1/incidents` and the postmortem index
  are only as complete as the humans who remember to write to them —
  the deploy gate refusing the unsafe path (`bdaecccc5`) is the
  higher-leverage fix.
