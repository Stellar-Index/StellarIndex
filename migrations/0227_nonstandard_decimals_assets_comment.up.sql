-- 0227 up — correct the stored comment on nonstandard_decimals_assets.
--
-- 0093 told a `\d+` reader that a row makes the API DECLINE price endpoints
-- for the pair, so a row only withholds data and is safe to prune. The
-- serving paths now normalise instead: every price-shaped read resolves the
-- leg's decimals through aggregate.ResolveDecimals, which falls back to 7
-- when no row exists, and corrects with aggregate.AdjustPrice. Deleting a
-- row therefore makes the API serve that token's pairs skewed by
-- 10^(decimals-7) until the guard re-confirms it. 0093 also cited a runbook
-- that does not exist; the section is in docs/operations/runbooks/dex.md.
--
-- Why a migration: COMMENT ON strings live in pg_description and 0093's up
-- body is immutable (migrations/README.md, "Amending a shipped migration").
--
-- Catalog-only: no heap, index or chunk is touched and nothing in Go reads
-- a catalog comment, so it is old-binary-safe (rule 9).

BEGIN;

SET LOCAL lock_timeout = '5s';

COMMENT ON TABLE nonstandard_decimals_assets IS
    'Confirmed non-7-decimal Soroban assets (internal/decimalsguard sweep). '
    'Each row DRIVES the read-time price correction: price, VWAP, OHLC, '
    'history and catalogue USD paths resolve the leg''s decimals here '
    '(aggregate.ResolveDecimals, 7 when absent) and normalise with '
    'aggregate.AdjustPrice. Deleting a row makes the API serve that '
    'token''s pairs skewed by 10^(decimals-7); never prune a row by hand. '
    'See docs/operations/runbooks/dex.md#dex-nonstandard-decimals.';

COMMIT;
