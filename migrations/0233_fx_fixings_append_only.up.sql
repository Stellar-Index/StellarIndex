-- 0233 up — make fx_fixings append-only in the database, and correct
-- 0193's claim that ingested_at is in no read predicate.
--
-- fx_fixings is the audit trail of every served fiat conversion (ADR-0018:
-- a closed bucket converts identically on every re-read). 0193 called it
-- append-only, but only writer discipline held that: the API role holds
-- UPDATE and DELETE through 0213's grant list. A BEFORE trigger now
-- refuses UPDATE, DELETE and TRUNCATE, as 0179 does for audit_log; a
-- correction is a new row at generation max+1. INSERT ... ON CONFLICT DO
-- NOTHING, the only write any binary issues, fires no UPDATE trigger.
--
-- ingested_at IS a read predicate: the API cache's delta load re-reads
-- rows with ingested_at > (previous load - FXFixingIngestSlack). A row
-- that commits more than that slack after its clock_timestamp() stamp is
-- never picked up by a warm cache, so every writer must go through
-- InsertFXFixingBatch, whose statement and context timeouts hold the
-- bound. The column comment now says so.
--
-- Lock: CREATE TRIGGER takes SHARE ROW EXCLUSIVE on fx_fixings and each
-- chunk; the table is small (FX bars), so this is sub-second and blocks
-- only the forex worker's insert for that time. No data is read or
-- rewritten; compressed chunks stay compressed.
-- Re-run: one transaction, so a failed apply leaves nothing behind and a
-- retry starts clean; golang-migrate never re-applies a recorded version.
-- Old-binary-safe (rule 9): the previous binary only INSERTs.

BEGIN;

CREATE OR REPLACE FUNCTION fx_fixings_append_only() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'fx_fixings is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege',
              HINT = 'Insert a correction at generation max+1 for the same (ticker, grain, bar_start).';
END;
$$;

COMMENT ON FUNCTION fx_fixings_append_only() IS
    'Refuses UPDATE, DELETE and TRUNCATE of fx_fixings: the table is the '
    'audit trail of served FX conversions.';

CREATE TRIGGER fx_fixings_append_only  -- migration-compat:ok no released binary updates or deletes fx_fixings; its one writer is INSERT ... ON CONFLICT DO NOTHING
    BEFORE UPDATE OR DELETE ON fx_fixings
    FOR EACH ROW EXECUTE FUNCTION fx_fixings_append_only();

CREATE TRIGGER fx_fixings_no_truncate  -- migration-compat:ok no released binary truncates fx_fixings
    BEFORE TRUNCATE ON fx_fixings
    FOR EACH STATEMENT EXECUTE FUNCTION fx_fixings_append_only();

COMMENT ON COLUMN fx_fixings.ingested_at IS
    'Write stamp. Not in any key, but the API cache''s delta load filters on '
    'it (ingested_at > previous load - FXFixingIngestSlack), so a writer must '
    'commit within that slack of the stamp: write only through '
    'InsertFXFixingBatch.';

COMMIT;
