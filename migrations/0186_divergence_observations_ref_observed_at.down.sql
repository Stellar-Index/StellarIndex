-- 0186 down — drop divergence_observations.ref_observed_at. Local/dev
-- iteration only (README rule 9): the per-reference observation times it
-- held are not recoverable from any other column.

ALTER TABLE divergence_observations
    DROP COLUMN IF EXISTS ref_observed_at;
