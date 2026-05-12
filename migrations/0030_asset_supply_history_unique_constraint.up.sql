-- 0030 up — convert asset_supply_history_asset_ledger_idx from a
-- UNIQUE INDEX to a UNIQUE CONSTRAINT so the supply-snapshot writer's
-- `ON CONFLICT (asset_key, ledger_sequence, time) DO NOTHING` can
-- match it on Timescale hypertables.
--
-- F-1205 follow-up (codex audit-2026-05-12): the supply-snapshot
-- timer test-fire on R1 surfaced
-- `there is no unique or exclusion constraint matching the
--  ON CONFLICT specification`. The unique INDEX migration 0005
-- created works for SELECT-side queries but column-inference
-- (`ON CONFLICT (cols)`) can't find it on the hypertable in
-- PG 16 + Timescale 2.16 — Postgres's ON CONFLICT inference
-- requires the constraint to be visible via pg_constraint, and
-- UNIQUE INDEX entries are not.
--
-- Timescale REJECTS the cheaper `ADD CONSTRAINT … USING INDEX`
-- form on hypertables (`hypertables do not support adding a
-- constraint using an existing index`, verified on r1). So we
-- drop the index and ADD CONSTRAINT, which builds a new index
-- under the constraint. The table is small (one row per
-- (asset, ledger) snapshot — single-digit-MB on r1 today), so
-- the rebuild cost is bounded.
--
-- Timescale supports UNIQUE constraints on hypertables as long
-- as the columns include the partitioning column (time).
-- `(asset_key, ledger_sequence, time)` does include time, so
-- the constraint creates cleanly.

-- Wrap in a transaction so a concurrent INSERT can't slip a
-- duplicate through between DROP INDEX and ADD CONSTRAINT.
BEGIN;

DROP INDEX IF EXISTS asset_supply_history_asset_ledger_idx;

ALTER TABLE asset_supply_history
    ADD CONSTRAINT asset_supply_history_asset_ledger_idx
    UNIQUE (asset_key, ledger_sequence, time);

COMMIT;
