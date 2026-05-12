-- 0030 down — undo the constraint promotion. The DROP CONSTRAINT
-- leaves the underlying unique index in place (Postgres does NOT
-- drop an index when its associated constraint is dropped if the
-- index was originally promoted from an existing index — but the
-- behaviour is actually to drop both, see PG docs §5.3.5). To be
-- safe we explicitly re-create the unique index after dropping the
-- constraint so the schema settles in the pre-0030 state.

ALTER TABLE asset_supply_history
    DROP CONSTRAINT asset_supply_history_asset_ledger_idx;

CREATE UNIQUE INDEX IF NOT EXISTS asset_supply_history_asset_ledger_idx
    ON asset_supply_history (asset_key, ledger_sequence, time);
