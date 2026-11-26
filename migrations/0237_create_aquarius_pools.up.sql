-- 0237 up — `aquarius_pools`: pool → the pool type its router add_pool
-- announced, for the aquarius decoder's liquidity layout.
--
-- protocol_contracts stores a contract SET only. A pool learned from a live
-- add_pool was warmed after a restart with no layout, so its deposit and
-- withdraw events were refused until it was added to an in-code list. This
-- table keeps the announced type (the raw Symbol, so a name this binary cannot
-- map is not lost). Rows are written only by the live indexer from an
-- add_pool emitted by the canonical router (the decoder's Matches gate).
-- Every row is trusted: the boot warm admits its pool into the gate, so a
-- hand-inserted row needs the same care as an in-code list entry.
-- New empty plain table: old-binary-safe. Never apply retention.

BEGIN;

CREATE TABLE IF NOT EXISTS aquarius_pools (
    contract_id  text        NOT NULL,
    pool_type    text        NOT NULL,
    first_ledger bigint      NOT NULL,
    observed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (contract_id)
);

COMMENT ON TABLE aquarius_pools IS
    'pool → raw add_pool type registry for the aquarius decoder, written from '
    'router add_pool events. Coverage-load-bearing: a pool in no in-code list '
    'and without a row has its deposit/withdraw events refused. Every row is '
    'trusted into the decoder gate at boot.';

COMMIT;
