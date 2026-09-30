-- 0191 up — drop 0001's inline `> 0` CHECKs on trades.base_amount and
-- trades.quote_amount so an SDEX fill with one leg rounded to zero
-- stroops can be stored.
--
-- The row invariant becomes: both legs >= 0 and not both zero. It is
-- enforced in Go by canonical.Trade.Validate on every writer (InsertTrade,
-- BatchInsertTrades via filterStorableTrades, the COPY bulk path) until a
-- later offline step restores a matching CHECK. 0187 already made every
-- trades-derived CAGG restrict its price expressions to priceable rows
-- (base_amount > 0 AND quote_amount > 0), so a zero-leg row cannot reach
-- a division or a price.
--
-- DROP CONSTRAINT is catalog-only and does not decompress: 0174's down
-- runs a DROP CONSTRAINT against recompressed chunks at 2.26.4 and
-- test/integration/migration_0191_trades_amount_checks_test.go asserts
-- every trades chunk is still compressed after this file. 0004's header
-- claim that DROP is rejected on a compressed hypertable predates 2.26.4.
--
-- Lock: ACCESS EXCLUSIVE on trades and its chunk catalog entries —
-- sub-second of work, but it queues behind any long reader, a
-- compression-policy run or a CAGG refresh, and every new trades query
-- queues behind it. No SET LOCAL lock_timeout here: the deploy runs
-- migrate under PGOPTIONS lock_timeout, and a longer value only lengthens
-- that queue. If the deploy's lock_timeout trips, golang-migrate leaves
-- schema_migrations DIRTY at 191 with the DDL rolled back; recover with
-- `stellarindex-migrate force 190` and re-run in a quiet window (pause
-- the trades compression policy if it keeps colliding).
--
-- Loosening only, old-binary-safe (rule 9): the previous binary's
-- Validate never writes a zero leg, so it runs unchanged on this schema.

BEGIN;

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_base_amount_check;
ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_quote_amount_check;

COMMIT;
