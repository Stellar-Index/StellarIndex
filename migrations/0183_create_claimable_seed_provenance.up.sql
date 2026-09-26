-- 0183 up — audit trail for `supply seed-claimable-balances` (GH #714).
--
-- The claimable seed wrote claimable_observations and left nothing behind
-- but a console summary, so nobody could tell whether an asset's claimable
-- component had ever been seeded from history, through which ledger, or by
-- a binary that retracts claimed balances. One row per classic asset,
-- upserted by a pass that finished with every row written and every
-- served-live balance resolved; a partial or failed pass stamps nothing.
--
-- New table only: nothing reads it, so the previous binary is unaffected
-- (rule 9).

BEGIN;

CREATE TABLE IF NOT EXISTS claimable_seed_provenance (
    -- supply.AssetKey CODE:ISSUER form, as in claimable_observations.
    asset_key             text        PRIMARY KEY,
    -- Live balances the pass seeded for this asset.
    claimables_seeded     integer     NOT NULL CHECK (claimables_seeded >= 0),
    -- Served-live balances the lake showed claimed, written as tombstones.
    claimables_retracted  integer     NOT NULL CHECK (claimables_retracted >= 0),
    -- Range of the seeded balances' own last-modified ledgers; NULL when
    -- the pass seeded no live balance for the asset.
    min_ledger_seen       bigint      CHECK (min_ledger_seen IS NULL OR min_ledger_seen >= 0),
    max_ledger_seen       bigint      CHECK (max_ledger_seen IS NULL OR max_ledger_seen >= 0),
    -- Ledger through which stellar.ledgers was proven contiguous and
    -- hash-linked before the reduction emitted anything.
    lake_verified_through bigint      NOT NULL CHECK (lake_verified_through >= 0),
    seeded_at             timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE claimable_seed_provenance IS
    'One row per classic asset recording the most recent complete '
    '`supply seed-claimable-balances` pass: balances seeded and retracted, '
    'and the ledger the lake was verified through. Audit trail only — not '
    'read by the supply computation.';

COMMIT;
