-- 0231 down — drop source_exclusions. Refuses while any row exists: the rows
-- are the operator record of which sources were excluded and when.

BEGIN;

SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM source_exclusions) THEN
        RAISE EXCEPTION '0231 down refused: source_exclusions holds rows; export them before dropping the table';
    END IF;
END;
$$;

DROP TABLE IF EXISTS source_exclusions;

COMMIT;
