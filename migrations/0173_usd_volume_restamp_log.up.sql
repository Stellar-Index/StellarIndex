-- 0173 up — `usd_volume_restamp_log`: the before-image of every in-place
-- `trades.usd_volume` rewrite made by `stellarindex-ops usd-volume-restamp`.
--
-- The restamp overwrites a served money column in place. Until now its
-- only trace was the run's `derive_generation` stamped on the row — which
-- says THAT a row was rewritten, not what it held before. Undoing a bad
-- run meant re-deriving the whole span.
--
-- Written by `timescale.Store.restampTradesUSDVolume`, the one path both
-- restamp writes (exact tier and the plan-based tiers) go through: in the
-- same REPEATABLE READ transaction as the UPDATE, one row per rewritten
-- trade, and the transaction refuses to commit unless the two counts
-- agree. A row carries the trade's primary key, its prior usd_volume and
-- derive_generation, and the value and generation the run wrote.
--
-- The run is identified by `derive_generation` (one per run — a resumed
-- run reuses it). Undoing a run restores, per trade, its EARLIEST log row
-- of that generation, and only while the trade is still at that run's
-- generation, so a later correction is never clawed back:
--
--   UPDATE trades t
--      SET usd_volume = l.prior_usd_volume,
--          derive_generation = l.prior_derive_generation
--     FROM (SELECT DISTINCT ON (source, ledger, tx_hash, op_index, ts) *
--             FROM usd_volume_restamp_log
--            WHERE derive_generation = $gen
--            ORDER BY source, ledger, tx_hash, op_index, ts, id) l
--    WHERE t.source = l.source AND t.ledger = l.ledger
--      AND t.tx_hash = l.tx_hash AND t.op_index = l.op_index AND t.ts = l.ts
--      AND t.derive_generation = $gen;
--
-- A plain table, not a hypertable, and no retention: it is an audit
-- record whose size is bounded by what operators choose to restamp.

BEGIN;

CREATE TABLE IF NOT EXISTS usd_volume_restamp_log (
    id                      bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    logged_at               timestamptz NOT NULL DEFAULT now(),
    source                  text        NOT NULL,
    ledger                  integer     NOT NULL,
    tx_hash                 char(64)    NOT NULL,
    op_index                integer     NOT NULL,
    ts                      timestamptz NOT NULL,
    prior_usd_volume        numeric,
    prior_derive_generation bigint      NOT NULL,
    usd_volume              numeric     NOT NULL,
    derive_generation       bigint      NOT NULL
);

CREATE INDEX IF NOT EXISTS usd_volume_restamp_log_generation_idx
    ON usd_volume_restamp_log (derive_generation);

COMMENT ON TABLE usd_volume_restamp_log IS
    'Before-image of every in-place trades.usd_volume rewrite by '
    'stellarindex-ops usd-volume-restamp: the trade key, the prior '
    'usd_volume/derive_generation and what the run wrote, logged in the '
    'same transaction as the UPDATE. derive_generation identifies the run.';

COMMIT;
