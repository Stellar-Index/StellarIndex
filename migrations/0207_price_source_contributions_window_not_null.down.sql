-- 0207 down — restore the nullable window column and the 0026 key.
-- CHECK (window_seconds > 0) stays: it already admits NULL.
--
-- The deleted pre-0169 rows are not restored. Re-adding the 0026 primary
-- key fails loudly if two windows now share a (pair, bucket, source);
-- delete the colliding rows first rather than leave the table keyless.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE price_source_contributions
    ALTER COLUMN window_seconds DROP NOT NULL;

ALTER TABLE price_source_contributions
    ADD PRIMARY KEY (asset_id, quote_id, bucket, source);

COMMENT ON COLUMN price_source_contributions.window_seconds IS
    'Aggregation window this breakdown was computed over, in whole seconds '
    '(300, 3600, 86400). NULL only on rows written before migration 0169, '
    'whose window was not recorded and cannot be recovered.';

COMMIT;
