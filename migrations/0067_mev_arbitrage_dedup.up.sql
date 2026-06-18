-- 0067 up — MEV: add the 'arbitrage' kind + a dedup key for idempotency.
--
-- The v1 MEV detector (internal/aggregate/mev) flags ATOMIC ARBITRAGE:
-- a single transaction in which one taker trades a closed asset cycle
-- (≥2 legs returning to a starting asset) across pools/venues. This is
-- the one MEV pattern our trade data supports unambiguously — the
-- `trades` served tier carries (ledger, tx_hash, op_index, taker) but
-- NOT intra-ledger transaction ordering, so cross-transaction sandwich
-- detection would be guesswork. The other kinds in the CHECK
-- (sandwich/oracle_deviation/liquidation_cascade/wash_trade) remain
-- reserved for when the lake-backed signals that disambiguate them land.
--
-- mev_events.event_id is a random UUID, so a re-scan of the same ledger
-- window would insert duplicate rows. dedup_key makes the detector's
-- writes idempotent: a UNIQUE index + ON CONFLICT DO NOTHING means the
-- worker can re-scan an overlapping window every tick without producing
-- duplicates. Shape: '<kind>:<tx_hash>:<taker>' (one detection per
-- (pattern, tx, actor)).

BEGIN;

ALTER TABLE mev_events DROP CONSTRAINT IF EXISTS mev_events_kind_check;
ALTER TABLE mev_events ADD CONSTRAINT mev_events_kind_check
    CHECK (kind IN ('sandwich', 'oracle_deviation', 'liquidation_cascade',
                    'wash_trade', 'arbitrage'));

ALTER TABLE mev_events ADD COLUMN IF NOT EXISTS dedup_key text;

CREATE UNIQUE INDEX IF NOT EXISTS mev_events_dedup_key_idx
    ON mev_events (dedup_key) WHERE dedup_key IS NOT NULL;

COMMENT ON COLUMN mev_events.dedup_key IS
    'Deterministic idempotency key (<kind>:<tx_hash>:<taker>). UNIQUE so '
    're-scanning a window is a no-op via ON CONFLICT DO NOTHING.';

COMMIT;
