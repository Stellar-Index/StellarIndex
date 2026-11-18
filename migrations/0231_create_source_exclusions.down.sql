-- 0231 down — drop source_exclusions and its append-only trigger function.
-- Refuses while any row exists: the rows are the operator record of which
-- sources were excluded and when.

BEGIN;

SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM source_exclusions) THEN
        RAISE EXCEPTION '0231 down refused: source_exclusions holds rows; export them before dropping the table';
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS source_exclusions_no_truncate ON source_exclusions;
DROP TRIGGER IF EXISTS source_exclusions_append_only ON source_exclusions;
DROP TABLE IF EXISTS source_exclusions;
DROP FUNCTION IF EXISTS source_exclusions_append_only();

COMMIT;
