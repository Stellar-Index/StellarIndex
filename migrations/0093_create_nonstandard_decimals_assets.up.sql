-- 0093 up — `nonstandard_decimals_assets`: the READ-SIDE table backing the
-- dex-nonstandard-decimals serving guard.
--
-- Context (CONFIRMED production bug, 2026-07-08): the served price is
-- Σ(quote_amount)/Σ(base_amount) on RAW smallest-unit integers — both in
-- the `prices_*` continuous aggregates (0002) and in `aggregate.VWAP`. The
-- per-asset decimals cancel in that ratio ONLY when base and quote share a
-- decimals scale. Token
-- CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO declares
-- decimals()=9, not the assumed 7, so every served price for its pairs
-- (aquarius CC2RB/USDC observed) is skewed exactly 100x — served 41.32 vs
-- true ~4132, live since 2026-06-22 (35 trades).
--
-- `internal/decimalsguard` (aggregator) already SWEEPS + CONFIRMS a
-- token's on-chain decimals() != 7 and raises
-- stellarindex_dex_trade_nonstandard_decimals_total — detection only, it
-- does not stop serving. This table is the WRITE target the guard upserts
-- into on confirmation; the API process mirrors it into an in-process
-- cache (internal/api/v1.NonstandardDecimalsCache, ~60s refresh) and
-- DECLINES to serve /v1/price, /v1/vwap, /v1/history, /v1/ohlc for any
-- pair with a leg present here (422, docs/operations/runbooks/
-- dex-nonstandard-decimals.md). Self-clearing: once the durable decimals
-- normalization ships and this row is removed (or simply left stale — the
-- guard stops re-confirming), the decline disappears within one cache
-- refresh interval.
--
-- Deliberately NOT a hypertable — this is a tiny, near-empty configuration/
-- control table (offenders should be ~0), not a time-series of ledger
-- events. No i128/NUMERIC concern either: `decimals` is a small on-chain
-- declared integer, not a token amount (ADR-0003 governs amounts, not
-- this).
--
-- Additive, old-binary-safe (migrations/README.md rule 9): a new
-- standalone table, no existing table/column/policy touched. A pre-0093
-- binary runs unchanged against a post-0093 schema (it simply never reads
-- or writes this table).
CREATE TABLE nonstandard_decimals_assets (
    -- Soroban token C-strkey contract id — the confirmed-offending leg.
    asset        text        PRIMARY KEY,
    -- The on-chain decimals() value the guard resolved from the certified
    -- lake. Constrained != 7 — a 7-dp confirmation is definitionally not an
    -- offender and has no reason to appear here.
    decimals     integer     NOT NULL CHECK (decimals >= 0 AND decimals <> 7),
    -- Which DEX connector's trade triggered the confirming sweep
    -- (soroswap / phoenix / aquarius / comet / …). Informational — the
    -- decline applies to the asset regardless of which source produced the
    -- pair being queried.
    source       text        NOT NULL,
    confirmed_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE nonstandard_decimals_assets IS
    'Confirmed non-7-decimal Soroban assets (internal/decimalsguard sweep). '
    'The API read-time serving guard (internal/api/v1.NonstandardDecimalsCache) '
    'declines /v1/price, /v1/vwap, /v1/history, /v1/ohlc for any pair with a '
    'leg listed here, rather than serve a price skewed by 10^(7-decimals). '
    'See docs/operations/runbooks/dex-nonstandard-decimals.md.';
