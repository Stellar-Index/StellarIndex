-- 0179 up — enforce audit_log's append-only property in the database.
--
-- WHAT IS WRONG TODAY
--
-- 0027 describes audit_log as append-only, but only in a comment. Nothing
-- stops the role that INSERTs the trail from rewriting or deleting it, so
-- one stray UPDATE, DELETE or TRUNCATE (or one injected statement) can
-- erase the record of who minted a key or lifted a freeze (GH #969).
--
-- WHAT THIS MIGRATION ADDS
--
--   audit_log_append_only()   trigger function; RAISEs on any UPDATE,
--                             DELETE or TRUNCATE of audit_log, with one
--                             exception below.
--   audit_log_append_only     BEFORE UPDATE OR DELETE, FOR EACH ROW.
--   audit_log_no_truncate     BEFORE TRUNCATE, FOR EACH STATEMENT.
--
-- THE ONE PERMITTED UPDATE. account_id and actor_user_id are
-- `ON DELETE SET NULL` foreign keys, and the signup reaper deletes
-- accounts (ReapSuspendedOrphans). Postgres implements SET NULL as an
-- UPDATE of the referencing row, so a trigger that refused every UPDATE
-- would make deleting any account or user with audit history fail. An
-- UPDATE is therefore allowed only when every column other than those two
-- is unchanged, and each of the two is either unchanged or set to NULL
-- while the row it pointed at no longer exists. That is exactly the
-- referential action and nothing more: the app cannot detach a row from
-- a live account or user, and cannot touch any other column. The column
-- comparison is over the whole row, so a column added later is protected
-- without editing this function.
--
-- WHY NO REVOKE. Migrations run as the `stellarindex` app role
-- (migrations/README.md rule 7), so that role OWNS audit_log. An owner can
-- re-grant itself anything, and the SET NULL action above runs with the
-- table owner's privileges: revoking UPDATE from it would break account
-- deletion without stopping a determined owner. The trigger is the
-- enforcing control. It stops DML — application bugs and injected
-- statements — not a session that can run ALTER TABLE ... DISABLE TRIGGER.
--
-- NO RETENTION PATH. There is no sanctioned delete. Retention/archival is
-- awaiting a decision on the retention period (0027's audit_log comment);
-- when that job lands, its migration replaces this function with its own
-- reviewed exception. Until then a delete fails closed.
--
-- Rule-9 safe: no released binary UPDATEs, DELETEs or TRUNCATEs audit_log
-- (postgresstore.AuditStore only INSERTs and SELECTs). Runtime: catalog
-- only, no table rewrite, no scan.

BEGIN;

CREATE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql
-- Resolve accounts/users where this migration found them, not wherever a
-- later session's search_path points.
SET search_path FROM CURRENT
AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND to_jsonb(NEW) - 'account_id' - 'actor_user_id'
           = to_jsonb(OLD) - 'account_id' - 'actor_user_id'
       AND (NEW.account_id IS NOT DISTINCT FROM OLD.account_id
            OR (NEW.account_id IS NULL
                AND NOT EXISTS (SELECT 1 FROM accounts WHERE id = OLD.account_id)))
       AND (NEW.actor_user_id IS NOT DISTINCT FROM OLD.actor_user_id
            OR (NEW.actor_user_id IS NULL
                AND NOT EXISTS (SELECT 1 FROM users WHERE id = OLD.actor_user_id)))
    THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege',
              HINT = 'The only permitted change is the ON DELETE SET NULL of a deleted account or user (migration 0179).';
END;
$$;

COMMENT ON FUNCTION audit_log_append_only() IS
    'Refuses UPDATE, DELETE and TRUNCATE of audit_log, except the ON DELETE '
    'SET NULL of account_id / actor_user_id once the referenced row is gone. '
    'Migration 0179, GH #969.';

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

COMMIT;
