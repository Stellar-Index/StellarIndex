-- 0191 down — restore 0001's `> 0` CHECKs on trades.base_amount and
-- trades.quote_amount. A development lever, not a production rollback:
-- rolling back the release is binary-only, because the loosened schema
-- is harmless to the previous binary.
--
-- REFUSES rather than deletes (LOUD, not silent) when:
--   * any trades row has a zero or negative leg — the restored CHECKs
--     would reject the table's own contents; delete those rows
--     explicitly first if that is really what you want;
--   * any trades chunk is compressed — TimescaleDB 2.26.4 cannot ADD a
--     CHECK over a compressed hypertable (see 0174's header); decompress
--     every chunk first, or leave the schema as 0191 left it.
--
-- The constraint names are the ones Postgres generated for 0001's inline
-- CHECKs, so a database that never ran 0191 and one that ran up+down are
-- indistinguishable.

BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM trades WHERE base_amount <= 0 OR quote_amount <= 0) THEN
    RAISE EXCEPTION '0191_trades_drop_amount_checks.down.sql: trades holds rows with a zero or negative leg — the > 0 CHECKs cannot be restored over them; down-migrating with data present is LOUD, not silent. Delete them explicitly first if that is really what you want.';
  END IF;
  IF EXISTS (SELECT 1 FROM timescaledb_information.chunks
              WHERE hypertable_name = 'trades' AND is_compressed) THEN
    RAISE EXCEPTION '0191_trades_drop_amount_checks.down.sql: trades has compressed chunks and TimescaleDB 2.26.4 cannot ADD a CHECK over them (see 0174) — LOUD, not silent. Decompress every trades chunk first, or keep the 0191 schema.';
  END IF;
END $$;

ALTER TABLE trades ADD CONSTRAINT trades_base_amount_check  CHECK (base_amount  > 0);
ALTER TABLE trades ADD CONSTRAINT trades_quote_amount_check CHECK (quote_amount > 0);

COMMIT;
