-- 0168 up — re-enable compression on asset_supply_history if 0030 left
-- it disabled (GH #1158).
--
-- 0030 disables compression, swaps the unique index for a constraint
-- inside an explicit BEGIN/COMMIT, and only re-enables compression in a
-- statement AFTER that COMMIT. golang-migrate sends the whole file as one
-- simple-protocol query, so the COMMIT ends the implicit transaction and
-- the trailing ALTER runs in a fresh one: if it fails, the disabled
-- compression is already durable and version 30 is dirty. `force 29`
-- cannot recover (0030's DROP INDEX then hits the constraint's index), so
-- the recovery is `force 30`, which records 0030 applied with compression
-- still off while 0005's compression policy keeps firing against it.
--
-- 0030's up body is immutable (migrations/README.md "Amending a shipped
-- migration"), so the chain converges here instead. Idempotent: on every
-- database where 0030 applied cleanly compression is already on and this
-- does nothing. The settings and the 7-day policy are 0005's.

BEGIN;

DO $$
BEGIN
    IF NOT (SELECT compression_enabled
              FROM timescaledb_information.hypertables
             WHERE hypertable_schema = current_schema()
               AND hypertable_name = 'asset_supply_history') THEN
        ALTER TABLE asset_supply_history SET (
            timescaledb.compress,
            timescaledb.compress_segmentby = 'asset_key',
            timescaledb.compress_orderby   = 'time DESC'
        );
        PERFORM add_compression_policy('asset_supply_history',
                                       INTERVAL '7 days',
                                       if_not_exists => true);
    END IF;
END
$$;

COMMIT;
