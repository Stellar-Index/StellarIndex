-- 0074 up — MEV: add the 'oracle_sandwich' kind.
--
-- The MEV worker now ships the detectors 0067 left reserved:
--   sandwich            — one account's trades in two transactions
--                         bracket another account's trade on the same
--                         pair within one ledger. Cross-transaction
--                         ordering (tx_index, application order) comes
--                         from the ClickHouse raw lake's
--                         stellar.tx_hash_index — the served trades
--                         table still doesn't carry it, which is why
--                         0067 called this "guesswork" from Postgres
--                         alone.
--   wash_trade          — self-crosses (maker == taker) + repeated
--                         two-account back-and-forth on one pair.
--   liquidation_cascade — Blend liquidation fills against distinct
--                         positions clustered within a short ledger
--                         window with an on-chain oracle update in the
--                         bracket.
--   oracle_sandwich     — NEW kind (this migration): one account's
--                         trades bracket an on-chain oracle update on
--                         an asset the trades touch, within one ledger
--                         (lake tx_index order). Distinct from the
--                         still-reserved 'oracle_deviation' (price
--                         divergence, not a positional pattern).
--
-- The first three were already in the CHECK; only oracle_sandwich
-- needs adding. Dedup keys (0067) are per-kind deterministic — see
-- internal/aggregate/mev for each detector's key shape.

BEGIN;

ALTER TABLE mev_events DROP CONSTRAINT IF EXISTS mev_events_kind_check;
ALTER TABLE mev_events ADD CONSTRAINT mev_events_kind_check
    CHECK (kind IN ('sandwich', 'oracle_deviation', 'liquidation_cascade',
                    'wash_trade', 'arbitrage', 'oracle_sandwich'));

COMMIT;
