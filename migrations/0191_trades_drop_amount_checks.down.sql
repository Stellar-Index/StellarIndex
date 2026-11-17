-- 0191 down — restore 0001's `> 0` CHECKs on trades.base_amount and
-- trades.quote_amount. A development lever, not a production rollback:
-- rolling back the release is binary-only, because the loosened schema
-- is harmless to the previous binary.
--
-- REFUSES rather than deletes (LOUD, not silent) when any trades row has a
-- zero or negative leg: the restored CHECKs would reject the table's own
-- contents; delete those rows explicitly first if that is really what you
-- want. Production holds such rows (0191 exists to store them), so there
-- the down refuses; golang-migrate is then dirty at 190 with the schema
-- still at 191, and `force 191` recovers it.
--
-- Compressed chunks need no decompression: TimescaleDB 2.26.4 validates an
-- ADD CONSTRAINT CHECK against compressed rows in place. It does scan every
-- row under ACCESS EXCLUSIVE on trades, so on a large table this is a
-- maintenance-window step.
--
-- The constraint names are the ones Postgres generated for 0001's inline
-- CHECKs, so a database that never ran 0191 and one that ran up+down are
-- indistinguishable.

BEGIN;

SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM trades WHERE base_amount <= 0 OR quote_amount <= 0) THEN
    RAISE EXCEPTION '0191_trades_drop_amount_checks.down.sql: trades holds rows with a zero or negative leg — the > 0 CHECKs cannot be restored over them; down-migrating with data present is LOUD, not silent. Delete them explicitly first if that is really what you want.';
  END IF;
END $$;

ALTER TABLE trades ADD CONSTRAINT trades_base_amount_check  CHECK (base_amount  > 0);
ALTER TABLE trades ADD CONSTRAINT trades_quote_amount_check CHECK (quote_amount > 0);

COMMIT;
