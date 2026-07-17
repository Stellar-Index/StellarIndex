-- 0111 down — drop `intra_ledger_seq` from the five balance-observation
-- hypertables, reverting the schema half of the C2-6 intra-ledger-order
-- guarded upsert.
--
-- The post-0111 binary references intra_ledger_seq in the five observation
-- writers, so a rollback of this migration must be paired with a rollback of
-- the binary to the pre-0111 (unguarded last-writer-wins) writers. DROP
-- COLUMN on a compressed hypertable is supported directly on TimescaleDB
-- 2.11+ (r1 runs 2.26).

BEGIN;

ALTER TABLE sac_balance_observations
    DROP COLUMN IF EXISTS intra_ledger_seq;

ALTER TABLE lp_reserve_observations
    DROP COLUMN IF EXISTS intra_ledger_seq;

ALTER TABLE claimable_observations
    DROP COLUMN IF EXISTS intra_ledger_seq;

ALTER TABLE trustline_observations
    DROP COLUMN IF EXISTS intra_ledger_seq;

ALTER TABLE account_observations
    DROP COLUMN IF EXISTS intra_ledger_seq;

COMMIT;
