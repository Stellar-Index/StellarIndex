-- 0200 up — finish what 0169 started: every price_source_contributions
-- row carries its aggregation window.
--
-- 0169 added window_seconds NULLABLE and left rows written before it NULL
-- (their window is unrecoverable and a breakdown with no window cannot be
-- told apart from its 5m/1h/24h siblings). This migration removes those
-- rows and makes the column mandatory:
--
--   1. delete every NULL-window row. Chunks that hold only such rows are
--      dropped whole (cheap on compressed data); the one chunk straddling
--      the cut-over gets a row DELETE.
--   2. window_seconds SET NOT NULL, CHECK (window_seconds > 0).
--   3. drop the 0026 primary key (asset_id, quote_id, bucket, source).
--      With the window in play it would REFUSE a 5m row whose bucket
--      collides with that pair's 1h row. The window-aware unique index
--      from 0169 is the only key from here on.
--
-- Old-binary-safe (README rule 9) only for a fleet already on the window
-- key: since 0169 shipped, the writer names ON CONFLICT (asset_id,
-- quote_id, window_seconds, source, bucket) and always sets the window. A
-- binary older than that INSERTs without window_seconds and would fail
-- with 23502, so do not apply this to an environment still running one.
-- The guard below refuses to delete when the NULL rows are not strictly
-- older than the first windowed row.
--
-- Run it off-peak: the guard and the chunk walk read the whole legacy
-- range once. Row locks only; the ACCESS EXCLUSIVE locks (drop_chunks,
-- ALTER TABLE) sit under lock_timeout so a stuck writer fails the
-- migration instead of queueing behind it.

BEGIN;

SET LOCAL lock_timeout = '5s';
-- The straddling chunk may be compressed; a filtered DELETE decompresses
-- it, which the default 100,000-tuple cap would abort.
SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0;

DO $$
DECLARE
    first_windowed timestamptz;
    last_null      timestamptz;
    c              record;
    cut            timestamptz := '-infinity';
BEGIN
    SELECT min(bucket) INTO first_windowed
      FROM price_source_contributions WHERE window_seconds IS NOT NULL;
    SELECT max(bucket) INTO last_null
      FROM price_source_contributions WHERE window_seconds IS NULL;

    IF last_null IS NULL THEN
        RETURN;
    END IF;

    IF first_windowed IS NULL OR last_null > first_windowed + INTERVAL '1 hour' THEN
        RAISE EXCEPTION
            '0200: NULL-window rows reach % but the first windowed row is %; a pre-0169 writer is still active (or none has run) - refusing to delete',
            last_null, first_windowed;
    END IF;

    FOR c IN
        SELECT range_start, range_end
          FROM timescaledb_information.chunks
         WHERE hypertable_name = 'price_source_contributions'
           AND range_end <= first_windowed
         ORDER BY range_start
    LOOP
        PERFORM drop_chunks('price_source_contributions',
                            older_than => c.range_end,
                            newer_than => c.range_start);
        cut := c.range_end;
    END LOOP;

    DELETE FROM price_source_contributions
     WHERE window_seconds IS NULL
       AND bucket >= cut
       AND bucket <= last_null;
END $$;

ALTER TABLE price_source_contributions
    ALTER COLUMN window_seconds SET NOT NULL;  -- migration-compat:ok every pre-0169 row is deleted above and the released writer always sets window_seconds

ALTER TABLE price_source_contributions
    DROP CONSTRAINT IF EXISTS price_source_contributions_window_seconds_check;

ALTER TABLE price_source_contributions
    ADD CONSTRAINT price_source_contributions_window_seconds_check  -- migration-compat:ok tightens only the NULL arm, which the NOT NULL above already excludes
    CHECK (window_seconds > 0);

ALTER TABLE price_source_contributions
    DROP CONSTRAINT IF EXISTS price_source_contributions_pkey;

COMMENT ON COLUMN price_source_contributions.window_seconds IS
    'Aggregation window this breakdown was computed over, in whole seconds '
    '(300, 3600, 86400).';

COMMIT;
