-- 0188 down — restore 0179's audit_log_append_only() verbatim and drop the
-- erasure helper and the slug tombstones. Local/dev iteration only (README
-- rule 9): the tombstones are not recoverable once dropped, so a slug an
-- erasure retired becomes reusable again.

BEGIN;

CREATE OR REPLACE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql
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

DROP FUNCTION IF EXISTS audit_log_erase_metadata(jsonb);

DROP TABLE IF EXISTS erased_account_slugs;

COMMIT;
