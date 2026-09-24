-- 0173 down — restore 0019's comment on divergence_observations.status
-- verbatim (0019_create_divergence_observations.up.sql), so `migrate
-- down` leaves pg_description byte-identical to the pre-0173 state.

COMMENT ON COLUMN divergence_observations.status IS
    'Whether |delta_pct| exceeded the per-(reference,pair) '
    'threshold at observation time. Distinct from the persistent '
    'flag returned by the API (which is "any reference firing").';
