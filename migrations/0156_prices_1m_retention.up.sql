-- 0156 up — attach a 90-day retention policy to `prices_1m`, and ship
-- it DISABLED.
--
-- ══════════════════════════════════════════════════════════════════
-- ⚠ THIS MIGRATION CREATES A POLICY THAT DELETES DATA EVERY DAY,
--   FOREVER. It is created `scheduled => false` and drops NOTHING
--   until an operator arms it deliberately (command below). The
--   migration REFUSES to commit if the policy is left armed.
--
--   Measured on r1 2026-09-07, arming it drops 77 of the view's 86
--   chunks — 22 GB spanning 2017-01-12..2026-05-07 — on the first
--   run, and thereafter everything that ages past 90 days, so the
--   view converges on the trailing window and the 69 GB becomes a
--   small multiple of one quarter's minute buckets.
--
--   ARM (a deliberate act, after reading this whole header):
--
--     SELECT alter_job(job_id, scheduled => true)
--       FROM timescaledb_information.jobs
--      WHERE proc_name = 'policy_retention'
--        AND hypertable_name = 'prices_1m';
--
--     SELECT job_id, scheduled, next_start
--       FROM timescaledb_information.jobs
--      WHERE proc_name = 'policy_retention'
--        AND hypertable_name = 'prices_1m';
--
--   DISARM (before ANY re-materialisation — see Recovery):
--
--     SELECT alter_job(job_id, scheduled => false)
--       FROM timescaledb_information.jobs
--      WHERE proc_name = 'policy_retention'
--        AND hypertable_name = 'prices_1m';
--
--     SELECT job_id, scheduled, next_start
--       FROM timescaledb_information.jobs
--      WHERE proc_name = 'policy_retention'
--        AND hypertable_name = 'prices_1m';
--
--   RUN THE SECOND STATEMENT EVERY TIME. `alter_job` over zero rows
--   is not an error: it prints nothing and exits 0, so a predicate
--   that matches nothing reads exactly like a successful disarm and
--   the policy drops 77 chunks the next day.
--
--   MATCH ON THE VIEW NAME, NEVER ON THE MATERIALIZATION HYPERTABLE.
--   `timescaledb_information.jobs` renders its `hypertable_name` as
--   COALESCE(ca.user_view_name, ht.table_name) joined on
--   ca.mat_hypertable_id = j.hypertable_id, and the retention API
--   stores the MAT hypertable id — so for a continuous aggregate the
--   view reports `prices_1m` and NEVER the underlying
--   `_materialized_hypertable_142`. A predicate resolved from the
--   continuous-aggregates catalogue's materialization-hypertable-name
--   column therefore selects NOTHING. Verified on r1 2026-09-07
--   against the structurally identical refresh job: that predicate
--   returns 0 rows, `hypertable_name = 'prices_1m'` returns job 1088.
--   The broken form is deliberately not written out here — a header
--   that contains it is a header somebody pastes.
-- ══════════════════════════════════════════════════════════════════
--
-- ── Why this is not migrations 0115 / 0147 ─────────────────────────
--
-- 0115 and 0147 dropped all seven price CAGGs and left
-- re-materialisation as a manual operator step, and it is still open
-- whether that destroyed history nobody rebuilt. The difference is
-- NOT that raw `trades` is permanent here and was not there: 0031
-- removed the retention on `trades` on 2026-05-14, before 0115
-- (2026-07-24) and 0147 (2026-08-22), so `trades` was already
-- permanent for both of them.
--
-- The difference runs the other way, and it is the reason this file
-- is written the way it is. 0115 and 0147 were ONE-OFF drops: they
-- left a static hole that a single re-materialisation closes for
-- good. This policy drops EVERY DAY, FOREVER — so a rebuild that is
-- not preceded by a disarm is undone by the next run, and every
-- recovery procedure below begins with the disarm rather than
-- mentioning it in passing.
--
-- ── Recovery: the FORCED form, not the plain one ───────────────────
--
--   1. DISARM (above), and CONFIRM with the verification SELECT.
--   2. Rebuild `prices_1m` in slices — the window is ~9 years of
--      minute buckets, so do not attempt it in one call:
--
--        CALL refresh_continuous_aggregate(
--               'prices_1m',
--               '<start>'::timestamptz, '<end>'::timestamptz,
--               force => true);
--
--   3. Only then, and only WINDOWED, rebuild the TWAP views — see
--      the next section, which is the dangerous part.
--
-- `force => true` is LOAD-BEARING and its absence is silent. A
-- retention policy drops chunks straight off the materialization
-- hypertable and writes NO entry in the materialization invalidation
-- log, and a plain refresh materialises only the ranges that log
-- names inside the window: over a dropped range it collects nothing,
-- prints `NOTICE: continuous aggregate "prices_1m" is already
-- up-to-date`, writes zero rows and exits 0. `force => true` injects
-- an invalidation spanning the whole requested window before the
-- scan, so the range is recomputed from `trades`. (TimescaleDB
-- 2.26.4 — r1's installed version — `tsl/src/continuous_aggs/
-- refresh.c: process_cagg_invalidations_and_refresh` and
-- `invalidation.c:
-- collect_and_delete_cagg_invalidations_in_window`. The `force`
-- argument is present on r1's
-- `refresh_continuous_aggregate(regclass, "any", "any", boolean,
-- jsonb)`.)
--
-- ── ⚠ TWAP: a NULL-start refresh after a drop DELETES its history ──
--
-- `_materialized_hypertable_142` is prices_1m's materialization
-- hypertable AND the RAW hypertable of both TWAP views. Measured on
-- r1 2026-09-07 in `_timescaledb_catalog.continuous_agg`: `twap_1h`
-- (mat 149) and `twap_1d` (mat 150) each carry
-- raw_hypertable_id = 142 AND parent_mat_hypertable_id = 142, and
-- `continuous_aggs_invalidation_threshold` holds a row for 142.
-- `ts_chunk_do_drop_chunks` calls `continuous_agg_invalidate_raw_ht`
-- for every chunk it drops, so ONE armed run writes 77 invalidation
-- entries against the TWAP views spanning 2017-01-12..2026-05-07.
--
-- Those entries are dormant under the daily refresh policies, which
-- only look back 4 hours (twap_1h) and 7 days (twap_1d). They are NOT
-- dormant under the command migrations 0081, 0126 and 0147 all
-- document:
--
--     DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());
--
-- Post-0156 that form processes the whole invalidation set against a
-- `prices_1m` whose old chunks are gone, and DELETES nine years of
-- TWAP history. Do not run it. A TWAP refresh must be WINDOWED —
-- explicit start, never NULL — and must come AFTER the `prices_1m`
-- force-rebuild for the same window, with the policy still disarmed:
--
--     CALL refresh_continuous_aggregate(
--            'twap_1h',
--            '<start>'::timestamptz, '<end>'::timestamptz,
--            force => true);
--
-- `prices_15m` and the other coarse rungs are NOT exposed to this:
-- they are built on `trades` (raw_hypertable_id = 1), not on
-- `prices_1m`.
--
-- ── Why this reverses migration 0031 for one aggregate ─────────────
--
-- 0031 removed the 30-day retention that migration 0002 had placed
-- on `prices_1m` / `prices_15m`, in the same transaction as the
-- 90-day policy on raw `trades`, on the reasoning that storage was
-- not a constraint and per-trade fidelity is unrecoverable from
-- aggregates. Both halves of that still hold and NEITHER is touched
-- here: `trades` keeps its forever retention, and `prices_15m` and
-- every coarser rung keep theirs. What changes is only the ONE
-- aggregate that is both the largest and the most redundant —
-- `prices_1m` is 69 GB over 86 chunks, 55 % of all price-CAGG
-- storage, against 27 GB for `prices_15m` and 13.6 GB for the four
-- coarsest combined — and it is the one rung fully recomputable from
-- a source 0031 itself guaranteed forever. 0031's own `down` stays a
-- no-op and is not the mechanism used here: this is the new,
-- reviewed FORWARD migration its comment asks for.
--
-- ── What a rebuild can and cannot restore ──────────────────────────
--
-- `trades` is intact and readable: 472 chunks spanning
-- 2017-01-12..2026-09-10, 313 compressed, and a
-- `time_bucket('1 minute', ts, 'UTC')` aggregate over a compressed
-- 2021 chunk returns rows in 5.5 ms. Compression is uneven — only 8
-- of the 52 chunks covering 2021 are compressed — which affects the
-- cost of a rebuild, not its outcome.
--
-- But `prices_1m` is ALREADY unmaterialised in places, and a rebuild
-- would restore MORE than was dropped rather than exactly it.
-- Measured on r1 2026-09-07: over 2021-11-09 12:00-13:00 UTC,
-- `trades` holds 244 rows and `prices_1m` holds 0 (while
-- `prices_15m` holds 4); over 2024-03-05 12:00-13:00 UTC, `trades`
-- holds 83 and `prices_1m` holds 0. So the view's present contents
-- are not a faithful minute-level record of `trades` today, and
-- "restore what retention dropped" and "materialise what the view
-- should hold" are different operations with different costs.
--
-- ── What loses historical reach ────────────────────────────────────
--
-- 60 non-test Go files name `prices_1m`. The widest TRAILING window
-- any of them reads is 7 days (`INTERVAL '7 days'`, the volume and
-- catalogue rollups), so every rollup, snapshot, alert and listing
-- path sits comfortably inside 90 days. Only the readers whose window
-- comes from the CALLER can reach past it, and beyond 90 days these
-- read an empty view — every price aggregate is `materialized_only`,
-- so there is no real-time fallback:
--
--   - /v1/ohlc?interval=1m — takes `from`/`to` verbatim, so a caller
--     CAN name a window older than 90 days and does get bars for it
--     today. ?interval=5m and ?interval=30m re-bucket the same view.
--   - /v1/history/since-inception?granularity=1m — no time bound at
--     all, ordered oldest bucket first, so the retention edge is the
--     FIRST thing it would return.
--   - /v1/chart?granularity=1m — already bounded to ~35 days of
--     minutes by the 50,000-point response cap, so it loses nothing a
--     caller could reach.
--   - twap_1h / twap_1d — see the TWAP section above.
--   - The ops re-derive tools (`usd_volume_restamp`,
--     `usd_volume_reconcile`) value historical rows against the
--     `prices_1m` bucket at each row's own timestamp, so a re-derive
--     over a range older than 90 days needs the rebuild first.
--
-- EXAMINED AND NOT AFFECTED, recorded so the next reader does not
-- have to re-derive it: /v1/price/at and /v1/price/changes. Both go
-- through `Store.ClosedVWAPAtOrBefore`, whose `priceAtResolutionLadder`
-- is keyed on the AGE of the requested instant and only probes
-- `prices_1m` for an instant within 48 hours — beyond that it reads
-- {1h, 4h, 1d}, and beyond 45 days {1d} alone. Their `window_seconds`
-- does degrade with age, but at 48 h and 45 days, for reasons that
-- predate this migration and are unchanged by it.
--
-- The 1m bound is stated on the served surfaces too: the OpenAPI
-- `granularity` and `interval` parameter descriptions, and the
-- explorer's window-to-grain table.
--
-- ── Nothing else is touched ────────────────────────────────────────
--
-- ONE aggregate. `prices_15m`, `prices_1h`, `prices_4h`,
-- `prices_1d`, `prices_1w`, `prices_1mo`, `twap_1h`, `twap_1d`,
-- every oracle_prices_* rung and raw `trades` keep no retention
-- policy at all. internal/storage/timescale/retention_policy_test.go
-- fails if this file ever names a second one, and it also pins the
-- disarm command above against the `hypertable_name` trap.

BEGIN;

SELECT add_retention_policy(
         'prices_1m',
         drop_after    => INTERVAL '90 days',
         if_not_exists => true);

-- Ship it DISABLED. Arming is a deliberate operator act, not a
-- side effect of a deploy: this repo's history already holds two
-- destructive CAGG migrations it regrets, and a policy that arms
-- itself on apply gives nobody a chance to read the header first.
SELECT alter_job(job_id, scheduled => false)
  FROM timescaledb_information.jobs
 WHERE proc_name = 'policy_retention'
   AND hypertable_name = 'prices_1m';

-- The disable above is the same predicate the header's disarm
-- command uses, and `alter_job` over zero rows exits 0 in silence —
-- so the migration ASSERTS the outcome rather than assuming it. A
-- wrong predicate, a renamed view or a changed jobs-view shape all
-- fail here instead of shipping an armed destructive policy.
DO $$
DECLARE
  total  integer;
  active integer;
BEGIN
  SELECT count(*), count(*) FILTER (WHERE scheduled)
    INTO total, active
    FROM timescaledb_information.jobs
   WHERE proc_name = 'policy_retention'
     AND hypertable_name = 'prices_1m';

  IF total <> 1 THEN
    RAISE EXCEPTION
      'expected exactly 1 retention policy on prices_1m, found %. The jobs view '
      'reports hypertable_name as the CAGG''s user view name; if that changed, every '
      'disarm command in this migration''s header matches zero rows and exits 0 in '
      'silence.', total;
  END IF;

  IF active <> 0 THEN
    RAISE EXCEPTION
      'the prices_1m retention policy is still SCHEDULED after the disable step; '
      'refusing to ship an armed destructive policy';
  END IF;
END $$;

COMMIT;
