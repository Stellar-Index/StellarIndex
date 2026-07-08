-- 0093 down — drop the nonstandard-decimals serving-guard table.
--
-- Local/dev iteration only — NOT a production rollback lever
-- (migrations/README.md rule 9). Dropping this in production while the
-- guard is actively declining a real offender would silently resume
-- serving a wrong price.
DROP TABLE IF EXISTS nonstandard_decimals_assets;
