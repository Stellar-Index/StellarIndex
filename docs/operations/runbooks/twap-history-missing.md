---
title: Runbook — TWAP history missing (post-migration refresh not run)
last_verified: 2026-08-15
status: ratified
severity: P3
---

# Runbook — `stellarindex_twap_history_missing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_twap_history_missing` |
| Severity | **P3** (ticket) |
| Detected by | `stellarindex_twap_history_missing{view} == 1` for > 2h |
| Emitted by | `data-freshness.sh` — compares each TWAP view's oldest bar against `prices_1m`'s oldest |
| Typical MTTR | minutes (one `CALL refresh_continuous_aggregate`) once `prices_1m` is whole |
| Impact | The API serves **no TWAP** for older ranges of `{view}` (`twap_1h`/`twap_1d`) while recent bars look healthy — silently-empty money history. |

## Symptoms

`stellarindex_twap_history_missing{view="twap_1h"} = 1` (or `twap_1d`).
`prices_1m` holds real back-history but the view's oldest materialized bar is
far newer, so it carries only the trailing window its refresh policy
auto-fills. Newest-bar freshness/age checks read **green** because the recent
sliver is present.

## Why this exists

The TWAP continuous aggregates are hierarchical roll-ups over `prices_1m`. A
migration that changes their SELECT must `DROP` + `CREATE` them `WITH NO DATA`
(the `0081 → 0115 → 0126` recreate pattern), which deletes all materialized
history. Re-materialization is a **manual operator step** that nothing
enforces:

```sql
CALL refresh_continuous_aggregate('twap_1h', NULL, now());
CALL refresh_continuous_aggregate('twap_1d', NULL, now());
```

Each view's refresh **policy** only auto-materializes a recent trailing window
(`twap_1h` `start_offset` 4h, `twap_1d` 7d), so recent bars reappear on the
next policy tick and hide the skipped follow-up. The ADR-0033 completeness
verdict does **not** cover this: `twap_*` are derived price CAGGs, not
reconcile targets.

## Quick diagnosis (≤ 5 min)

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT 'prices_1m' v, min(bucket) FROM prices_1m
  UNION ALL SELECT 'twap_1h', min(bucket) FROM twap_1h
  UNION ALL SELECT 'twap_1d', min(bucket) FROM twap_1d;"
```

If `twap_1h`/`twap_1d`'s `min(bucket)` trails `prices_1m`'s by more than a day
(or the view is empty), the follow-up did not run.

## Mitigation (≤ 15 min to start)

1. Confirm `prices_1m` is whole first — these are hierarchical CAGGs over it, so
   refreshing before `prices_1m` is complete just re-materializes gaps.
2. Re-materialize the affected view(s):

   ```sql
   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
   CALL refresh_continuous_aggregate('twap_1d', NULL, now());
   ```

   Budget minutes, not hours — these roll-ups are small relative to the seven
   `prices_*` views.
3. The gauge clears on the next `data-freshness.sh` tick (≤ 15 min) once the
   view's oldest bar again reaches back to `prices_1m`'s oldest.

## Known false-positive patterns

- A brand-new / freshly-restored deployment whose `prices_1m` has < 2 days of
  history is deliberately **not** judged (the detector's guard), so an
  in-progress initial materialization does not page.
- During a legitimate recreate deploy, `for: 2h` gives you time to run the
  refresh before the alert fires. If you ran the refresh, wait one tick and
  confirm it clears rather than silencing.

## Related

- `stellarindex_completeness_incomplete` — the ADR-0033 served<>lake verdict
  (covers source tables; does NOT cover these derived CAGGs, which is why this
  alert exists).
- `stellarindex_cagg_last_refresh_unix` / `cagg-stale.md` — the refresh-POLICY
  health probe (a different failure: the policy itself not running).
- Migrations `0081` / `0115` / `0126` (the TWAP-CAGG recreate lineage).

## Changelog

- 2026-08-15: created — closes the "migration emptied a TWAP CAGG, refresh
  follow-up unenforced/undetected" gap (audit-2026-08-14 W1-migrations-1 /
  REC-01).
