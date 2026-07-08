-- 0091: covering index for the /v1/protocols/{name} pool-token resolver.
--
-- PoolTokens' aquarius query (internal/storage/timescale/pool_tokens.go)
-- is:
--
--   SELECT DISTINCT ON (contract_id, token_index) contract_id, token_index, token
--     FROM aquarius_liquidity
--    WHERE token IS NOT NULL
--    ORDER BY contract_id, token_index, ledger_close_time DESC
--
-- None of migration 0089's indexes lead with (contract_id, token_index),
-- so Postgres materializes and sorts EVERY token-bearing row before it
-- can apply DISTINCT ON — a full explicit sort of a forever-retained
-- hypertable backing the busiest AMM (14.9k events/24h), sitting in the
-- protocol-detail request path (review 2026-07-08, #91 finding 2; the
-- 2026-06-19 protocol-detail runaway-query incident is the same class).
-- This index matches the ORDER BY exactly, so DISTINCT ON streams off an
-- index scan and stops early per group.
CREATE INDEX aquarius_liquidity_pool_token_idx
    ON aquarius_liquidity (contract_id, token_index, ledger_close_time DESC)
    WHERE token IS NOT NULL;
