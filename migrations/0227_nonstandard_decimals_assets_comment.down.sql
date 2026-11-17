-- 0227 down — restore 0093's table comment verbatim
-- (0093_create_nonstandard_decimals_assets.up.sql), so `migrate down`
-- leaves pg_description as it was before 0227.

COMMENT ON TABLE nonstandard_decimals_assets IS
    'Confirmed non-7-decimal Soroban assets (internal/decimalsguard sweep). '
    'The API read-time serving guard (internal/api/v1.NonstandardDecimalsCache) '
    'declines /v1/price, /v1/vwap, /v1/history, /v1/ohlc for any pair with a '
    'leg listed here, rather than serve a price skewed by 10^(7-decimals). '
    'See docs/operations/runbooks/dex-nonstandard-decimals.md.';
