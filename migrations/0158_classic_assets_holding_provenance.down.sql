-- Revert 0158: drop the trade/holding provenance columns and restore the
-- 0023 table comment.
--
-- ASYMMETRY, stated: dropping first_holding_* / last_holding_* discards
-- the holdings evidence the backfill gathered, and the `*_seen_*` columns
-- keep whatever the holdings writer widened them to (an UPDATE back to
-- first_trade_* is impossible once first_trade_* is gone, and doing it
-- before the drop would be wrong for any row that was legitimately widened
-- by a trade). Re-running `stellarindex-ops asset-registry-backfill`
-- after a re-up restores the holdings columns; `*_seen_*` needs no repair
-- because the union reading is still the correct one for those columns.

BEGIN;

DROP INDEX IF EXISTS classic_assets_no_trade_idx;
DROP INDEX IF EXISTS classic_assets_no_holding_idx;

ALTER TABLE classic_assets
    DROP COLUMN IF EXISTS first_trade_at,
    DROP COLUMN IF EXISTS first_trade_ledger,
    DROP COLUMN IF EXISTS last_trade_at,
    DROP COLUMN IF EXISTS last_trade_ledger,
    DROP COLUMN IF EXISTS first_holding_at,
    DROP COLUMN IF EXISTS first_holding_ledger,
    DROP COLUMN IF EXISTS last_holding_at,
    DROP COLUMN IF EXISTS last_holding_ledger;

COMMENT ON COLUMN classic_assets.first_seen_at IS NULL;
COMMENT ON COLUMN classic_assets.first_seen_ledger IS NULL;
COMMENT ON COLUMN classic_assets.last_seen_at IS NULL;
COMMENT ON COLUMN classic_assets.last_seen_ledger IS NULL;
COMMENT ON COLUMN classic_assets.observation_count IS NULL;

COMMENT ON TABLE classic_assets IS
    'Auto-populated registry of every classic asset observed on '
    'SDEX or in any other op. One row per (code, issuer); slug is '
    'the URL-safe identifier used by /coins/{slug}.';

COMMIT;
