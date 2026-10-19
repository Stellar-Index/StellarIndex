-- 0219 up — `sep41_supply_rollup.bound_ledger` / `bound_observed_at`: the close
-- time of the fold checkpoint, so the live-tail delta read can bound observed_at.
--
-- sep41_supply_events is chunked on observed_at but the delta read filters on
-- ledger, so every call planned all 144 chunks (77 ms idle, ~1 s under load on
-- r1). The fold records the max observed_at it folded beside the ledger it
-- belongs to; ledger close times strictly increase, so every row above that
-- ledger is at or after it. The reader applies the bound only while
-- bound_ledger = last_ledger: a writer that moves or resets last_ledger without
-- knowing these columns (an older binary) disables it instead of hiding rows.
-- Additive, nullable; old-binary-safe.

BEGIN;

ALTER TABLE sep41_supply_rollup
    ADD COLUMN bound_ledger      integer,
    ADD COLUMN bound_observed_at timestamptz;

COMMENT ON COLUMN sep41_supply_rollup.bound_observed_at IS
    'Max observed_at folded at bound_ledger; the delta read bounds observed_at with it only while bound_ledger = last_ledger.';

COMMIT;
