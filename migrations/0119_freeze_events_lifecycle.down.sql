-- 0119 down — drop the durable ADR-0019 freeze-lifecycle columns.
--
-- Destructive in a way worth stating plainly: these four columns are the
-- only copy of the freeze ladder that survives Redis. Reverting restores
-- the pre-0119 behaviour in which a Redis flush is indistinguishable from
-- an operator force-unfreeze, so every live freeze — including one that
-- has ESCALATED to operator review and per ADR-0019 "stays active until
-- manual unfreeze" — silently releases on the next aggregator tick and
-- republishes the price a P1 alert had already escalated.
--
-- Export the open ladder before reverting if anything is frozen:
--
--   SELECT asset_id, quote_id, frozen_at, hold_until,
--          extensions_used, escalated, corroborated
--     FROM freeze_events
--    WHERE recovered_at IS NULL;
--
-- The post-0119 aggregator WRITES these columns on every lifecycle
-- transition, so reverting must be paired with a rollback to a pre-0119
-- binary (migrations/README.md rule 9's two-release dance). Reverting the
-- migration alone leaves the freeze writer UPDATE-ing columns that no
-- longer exist on the path that decides whether a manipulated price is
-- published.
--
-- The freeze itself is not lost by this: `frozen_at` / `recovered_at` /
-- `reason` / `frozen_value` are untouched, so the /v1/anomalies timeline
-- keeps its history. Only the lifecycle state goes.

BEGIN;

ALTER TABLE freeze_events
    DROP COLUMN IF EXISTS corroborated,
    DROP COLUMN IF EXISTS escalated,
    DROP COLUMN IF EXISTS extensions_used,
    DROP COLUMN IF EXISTS hold_until;

COMMIT;
