-- 0153 down — drop the issuer auth-flag provenance columns.
--
-- Fully reverses the up: the two CHECK constraints and the two COMMENTs are
-- attached to the columns and go with them, and the up's backfill only ever
-- wrote auth_flags_source (a column that no longer exists afterwards), so no
-- pre-existing value is touched. auth_required/revocable/immutable/clawback
-- and home_domain are left exactly as they were.
--
-- LOSSY, deliberately and only in one direction: any row the drain resolved
-- from a MERGED account keeps its flags but loses the label saying they are
-- historical, so re-applying 0153 afterwards would stamp them 'live' — the
-- backfill's "every persisted row came from a live entry" premise is true of
-- pre-0153 rows, not of rows written after it. Down-migrate this one only on
-- a local/dev database (migrations rule 9: downs are an iteration lever, not
-- a production rollback path); on a real deployment, re-run
-- `stellarindex-ops issuer-flags` after re-applying so provenance is
-- re-derived rather than assumed.

BEGIN;

ALTER TABLE issuers DROP COLUMN IF EXISTS auth_flags_as_of_ledger;
ALTER TABLE issuers DROP COLUMN IF EXISTS auth_flags_source;

COMMIT;
