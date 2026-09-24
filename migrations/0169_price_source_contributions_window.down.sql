-- 0169 down — drop the window discriminator and the retention policy.
--
-- Destroys window_seconds on every row written since 0169; the 0026
-- primary key is untouched by the up, so no key needs restoring. The
-- table/column comments revert to 0026's text.

BEGIN;

SELECT remove_retention_policy('price_source_contributions', if_exists => true);

DROP INDEX IF EXISTS price_source_contributions_window_key;

ALTER TABLE price_source_contributions
    DROP COLUMN IF EXISTS window_seconds;

COMMENT ON COLUMN price_source_contributions.bucket IS NULL;

COMMENT ON TABLE price_source_contributions IS
    'Per-closed-bucket source breakdown of every VWAP. Powers the '
    'source-contribution donut on every price card.';

COMMIT;
