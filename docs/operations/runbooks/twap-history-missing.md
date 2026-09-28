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
| Emitted by | `data-freshness.sh` — compares each view's oldest bar against the oldest `trades` row (or an armed retention policy's floor) |
| Typical MTTR | minutes (one WINDOWED `CALL refresh_continuous_aggregate` per view) once its source is whole |
| Impact | The API serves **no bars** for older ranges of `{view}` (`twap_1h`/`twap_1d`, or a `prices_*` view via `stellarindex_cagg_history_missing`) while recent bars look healthy — silently-empty money history. |

## Symptoms

`stellarindex_twap_history_missing{view="twap_1h"} = 1` (or `twap_1d`), or
`stellarindex_cagg_history_missing{view="prices_*"} = 1`. `trades` holds real
back-history but the view's oldest materialized bar is far newer, so it
carries only the trailing window its refresh policy auto-fills. Newest-bar
freshness/age checks read **green** because the recent sliver is present.

## Why this exists

The TWAP continuous aggregates are hierarchical roll-ups over `prices_1m`. A
migration that changes their SELECT must `DROP` + `CREATE` them `WITH NO DATA`
(the `0081 → 0115 → 0126 → 0147` recreate pattern), which deletes all
materialized history. Re-materialization is a **manual operator step** that
nothing enforces — and it must be **windowed**. See the warning below.

Each view's refresh **policy** only auto-materializes a recent trailing window
(`twap_1h` `start_offset` 4h, `twap_1d` 7d), so recent bars reappear on the
next policy tick and hide the skipped follow-up. The ADR-0033 completeness
verdict does **not** cover this: `twap_*` are derived price CAGGs, not
reconcile targets.

### ⚠ Never refresh a TWAP view with a NULL start

`twap_1h` and `twap_1d` are built on `prices_1m`, and migration 0156 attaches
a retention policy to `prices_1m`. Every chunk that policy drops writes an
invalidation against both TWAP views (77 of them, 2017–2026, on r1's first
armed run). A refresh whose start is NULL processes all of them against a
`prices_1m` whose old chunks are gone and **deletes the TWAP history** for
every range retention has dropped. The form is quoted here so you recognise
it, never so you run it:

```sql
-- DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());
```

Every refresh below names an explicit start at its source's oldest bar and
ends at the view's current oldest bar (or, to reach the minimum refresh
width, later), so it only writes over a range its source still holds.
`scripts/ci/lint-migration-commands.sh` fails the tree if the NULL-start form
appears unmarked in a runbook, an alert rule or an ops script.

## Quick diagnosis (≤ 5 min)

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT 'trades' v, min(ts) FROM trades
  UNION ALL SELECT 'prices_1m', min(bucket) FROM prices_1m
  UNION ALL SELECT 'twap_1h', min(bucket) FROM twap_1h
  UNION ALL SELECT 'twap_1d', min(bucket) FROM twap_1d;"
sudo -u postgres psql -d stellarindex -c \
 "SELECT job_id, scheduled FROM timescaledb_information.jobs
   WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m';"
```

If the view's `min(bucket)` trails its source's by more than a day (or the
view is empty), the follow-up did not run. The source is `prices_1m` for the
TWAP views and `trades` for the `prices_*` views. The second query tells you
whether `prices_1m`'s retention is armed (`scheduled = t`).

## Mitigation (≤ 15 min to start)

1. **TWAP views: settle the `prices_1m` window first.** A TWAP view can only be
   rebuilt over the range `prices_1m` still holds. If the retention policy is
   armed and the TWAP history you need is older than `prices_1m`'s
   `min(bucket)`, stop here and follow the Recovery section of
   `migrations/0156_prices_1m_retention.up.sql` in order: DISARM and confirm,
   force-rebuild `prices_1m` in slices for that window, and only then return to
   step 2 with the disarm still in force.
2. **Re-materialize the empty range, windowed.** `<start>` is the source's
   `min(bucket)` (TWAP) or `min(ts)` (`prices_*`) from the diagnosis; `<end>` is
   the view's current `min(bucket)`, or `now()` if the view is empty. The
   window must be at least the view's minimum refresh width or TimescaleDB
   rejects it with `22023 refresh window too small`: 3 h for
   `twap_1h`/`prices_1h`, 3 days for `twap_1d`/`prices_1d`, 21 days for
   `prices_1w`, 93 days for `prices_1mo` (the `MinWindow` column of
   `TradesCAGGs` in `internal/storage/timescale/diagnostics.go`). If it is
   shorter, move `<end>` later, toward `now()` — never `<start>` earlier,
   which for a TWAP view reaches into the range retention dropped:

   ```sql
   CALL refresh_continuous_aggregate(
          'twap_1h',
          '<start>'::timestamptz, '<end>'::timestamptz,
          force => true);
   ```

   Repeat for `twap_1d` (or the named `prices_*` view). `force => true` is
   required: over a range retention dropped, a plain refresh reports
   `already up-to-date` and writes nothing (0156, "Recovery"). Budget minutes
   for the TWAP views; a `prices_*` view over years of `trades` takes hours —
   run it in slices of a few months.
3. The gauge clears on the next `data-freshness.sh` tick (≤ 15 min) once the
   view's oldest bar again reaches back to its floor. With `prices_1m`'s
   retention armed, a TWAP view's floor is still the oldest trade, so the TWAP
   gauge keeps firing until step 1's rebuild has run.

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
- Migrations `0081` / `0115` / `0126` / `0147` (the TWAP-CAGG recreate
  lineage) and `0156` (the `prices_1m` retention that makes a NULL-start TWAP
  refresh destructive).

## Changelog

- 2026-08-15: created — closes the "migration emptied a TWAP CAGG, refresh
  follow-up unenforced/undetected" gap (audit-2026-08-14 W1-migrations-1 /
  REC-01).
- 2026-09-23: the NULL-start re-materialization this runbook prescribed deletes
  TWAP history once 0156's `prices_1m` retention is armed. Replaced with the
  windowed, `force => true` form, bounded above by the view's oldest bar, and
  gated on the 0156 recovery order. The alert annotation in both rule trees
  changed to match.
- 2026-09-28: step 2 names each view's minimum refresh width; a window
  narrower than it fails `22023 refresh window too small`.
