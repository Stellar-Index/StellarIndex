-- 0236 up — cap nonstandard_decimals_assets.decimals at 38.
--
-- 0093 bounds decimals only below (>= 0, <> 7). The served price correction
-- multiplies by 10^|base-quote| from this column, so a hand-seeded 180 for 18
-- serves every price for that token's pairs 10^162 off, and a pasted ledger
-- sequence makes each request compute a multi-megabyte big.Int power. 38 is
-- the ceiling the lake reader already enforces (maxSaneTokenDecimals) and
-- oracle_updates.decimals carries (0003).
--
-- Compat: the only Go writer takes decimals from the lake reader, which
-- rejects anything above 38, so v0.129.0 writes no row this rejects. A row
-- already above 38 fails VALIDATE and stops the migration: correct or delete
-- it (pre-check: docs/operations/runbooks/dex.md, dex-nonstandard-decimals,
-- step 6) and re-run; no value is guessed here.
--
-- Lock: ADD CONSTRAINT and VALIDATE run in one transaction, so the ACCESS
-- EXCLUSIVE lock ADD takes is held until COMMIT; the table holds one row per
-- nonstandard-decimals asset (tens), a scan of milliseconds.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE nonstandard_decimals_assets
    DROP CONSTRAINT IF EXISTS nonstandard_decimals_assets_decimals_max;  -- migration-compat:ok drops only this migration's own constraint, for a re-run; no binary names it

ALTER TABLE nonstandard_decimals_assets
    ADD CONSTRAINT nonstandard_decimals_assets_decimals_max  -- migration-compat:ok the only Go writer takes decimals from the lake reader, which rejects values above 38 (maxSaneTokenDecimals)
    CHECK (decimals <= 38) NOT VALID;

ALTER TABLE nonstandard_decimals_assets
    VALIDATE CONSTRAINT nonstandard_decimals_assets_decimals_max;

COMMIT;
