-- 0177 up — account_directory.override_by: WHO lifted a scam flag
-- (GH #858).
--
-- Migration 0170 made an operator override carry its reason; it still
-- did not say who decided. A row with source = 'operator-override'
-- republishes a flagged issuer's price, lifts its assets out of the
-- bottom rank tier and removes the explorer's flag pill, so a reviewer
-- needs a person to ask as well as a why. `stellarindex-ops
-- directory-override -clear-scam-flag` writes -actor (default: the OS
-- login) here, the same identity rule the key commands record in
-- audit_log.
--
-- The CHECK mirrors account_directory_override_reason_chk: an override
-- row must name a non-blank operator and an upstream row must name
-- none. Override rows written before this migration are backfilled with
-- an explicit placeholder so the constraint validates; that placeholder
-- is the marker to grep for when re-attributing them.
ALTER TABLE account_directory
    ADD COLUMN IF NOT EXISTS override_by text;

UPDATE account_directory
   SET override_by = 'unrecorded (override predates migration 0177)'
 WHERE source = 'operator-override'
   AND (override_by IS NULL OR btrim(override_by) = '');

ALTER TABLE account_directory
    DROP CONSTRAINT IF EXISTS account_directory_override_by_chk;
ALTER TABLE account_directory
    ADD CONSTRAINT account_directory_override_by_chk CHECK (  -- migration-compat:ok no released binary writes operator-override rows (directory-override is unreleased); released binaries write only upstream rows, override_by NULL
        CASE WHEN source = 'operator-override'
             THEN override_by IS NOT NULL AND btrim(override_by) <> ''
             ELSE override_by IS NULL
        END
    );
