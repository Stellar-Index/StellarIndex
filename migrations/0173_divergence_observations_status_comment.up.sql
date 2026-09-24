-- 0173 up — correct the stored comment on divergence_observations.status.
--
-- 0019 told a `\d+` reader two false things about this column:
--
--   1. that the threshold is per-(reference, pair). The worker compares
--      every reference against ONE service-wide threshold
--      (divergence.threshold_pct, default 5); there is no per-reference
--      or per-pair override.
--   2. that the API's persistent flag is "any reference firing". The
--      flag (flags.divergence_warning, divergence.CachedResult's
--      WarningFired, cached at div:<base>/<quote>) needs a quorum of
--      answering references (min_sources_for_warning, default 2), a
--      median-vs-our-price breach or zero agreeing references, and a
--      debounce (WarningPersistence, default 5 min across at least two
--      refreshes). A single firing row with the flag off is normal, so an
--      operator who inferred the flag from these rows was misled.
--
-- Why a migration: a COMMENT ON string lives in pg_description, not in
-- the repo. 0019's up body is immutable and editing it would reach only
-- a fresh database (migrations/README.md, "Amending a shipped
-- migration"; 0151 is the precedent).
--
-- Catalog-only: no heap, index or chunk is touched, and nothing in Go
-- reads a catalog comment, so it is old-binary-safe (rule 9).

COMMENT ON COLUMN divergence_observations.status IS
    'Per-reference echo: firing when this one reference''s |delta_pct| '
    'exceeded the worker''s single threshold (divergence threshold_pct, '
    'default 5) at observation time, clear otherwise. NOT the API flag: '
    'flags.divergence_warning (CachedResult.WarningFired, Redis key '
    'div:<base>/<quote>) additionally needs min_sources_for_warning '
    'answering references (default 2), a median breach or zero agreeing '
    'references, and a WarningPersistence debounce (default 5 min across '
    'at least two refreshes). Firing rows with the flag off are normal; '
    'read the Redis blob, never these rows, for the flag.';
