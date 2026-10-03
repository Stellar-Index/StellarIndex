-- 0200 up — `sushiswap_v3_pools`: pool → token identities for the
-- sushiswap_v3 decoder (soroswap_pairs shape, migration 0016).
--
-- protocol_contracts stores a contract SET only. A pool admitted through
-- that warm was gated in with no token mapping until its `pool_created`
-- event was replayed, so its swaps failed closed. This table carries what
-- the factory's creation event announced, so a restart resumes with the
-- full mapping. Rows are written only from a pool_created event emitted by
-- a trusted factory (the decoder's Matches gate); never apply retention.

BEGIN;

CREATE TABLE IF NOT EXISTS sushiswap_v3_pools (
    pool_id         text        NOT NULL,
    factory_id      text        NOT NULL,
    token0          text        NOT NULL,
    token1          text        NOT NULL,
    fee_pips        integer     NOT NULL, -- lint-money:ok fee tier in hundredths of a bp, not an amount
    tick_spacing    integer     NOT NULL,
    creation_ledger bigint      NOT NULL,
    observed_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pool_id)
);

COMMENT ON TABLE sushiswap_v3_pools IS
    'pool → (token0, token1, fee) registry for the sushiswap_v3 decoder, '
    'written from factory pool_created events. Coverage-load-bearing: '
    'without a row a gated pool''s swaps are dropped and counted.';

COMMIT;
