-- 0230 down — restore 0223's audit_log_erase_metadata and 0188's
-- audit_log_append_only verbatim. Local/dev iteration only (README rule 9):
-- rows an erasure scrubbed while 0230 was applied keep the wider scrub.

BEGIN;

SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION audit_log_erase_metadata(m jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE
AS $$
    SELECT CASE
        WHEN m IS NULL OR jsonb_typeof(m) <> 'object' THEN m
        ELSE (m - k)
            || CASE WHEN jsonb_typeof(m -> 'before') = 'object'
                    THEN jsonb_build_object('before', (m -> 'before') - k)
                    ELSE '{}'::jsonb END
            || CASE WHEN jsonb_typeof(m -> 'after') = 'object'
                    THEN jsonb_build_object('after', (m -> 'after') - k)
                    ELSE '{}'::jsonb END
    END
    FROM (SELECT ARRAY['account_slug', 'target_identifier', 'actor_identifier',
                       'label', 'name', 'suspended_reason', 'email',
                       'url_host', 'previous_url_host',
                       'target_account', 'credential_name']::text[] AS k) keys
$$;

COMMENT ON FUNCTION audit_log_erase_metadata(jsonb) IS
    'audit_log metadata with the subject-identifying keys removed (top level '
    'and inside before/after). The only metadata an account erasure may '
    'write (migrations 0188 and 0223).';

CREATE OR REPLACE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
DECLARE
    erasing text := current_setting('stellarindex.erasing_account', true);
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
    IF TG_OP = 'UPDATE'
       AND erasing IS NOT NULL AND erasing <> ''
       AND to_jsonb(NEW) - 'metadata' - 'ip' - 'user_agent'
           = to_jsonb(OLD) - 'metadata' - 'ip' - 'user_agent'
       AND NEW.metadata IS NOT DISTINCT FROM audit_log_erase_metadata(OLD.metadata)
       AND (NEW.ip IS NOT DISTINCT FROM OLD.ip
            OR (NEW.ip IS NULL AND OLD.actor_kind <> 'staff'))
       AND (NEW.user_agent IS NOT DISTINCT FROM OLD.user_agent
            OR (NEW.user_agent IS NULL AND OLD.actor_kind <> 'staff'))
       AND EXISTS (
            SELECT 1 FROM accounts a
             WHERE a.id = erasing::uuid
               AND a.status = 'closed'
               AND (OLD.account_id = a.id
                    OR (OLD.target_kind = 'account' AND OLD.target_id = a.id::text)
                    OR OLD.actor_user_id IN (SELECT u.id FROM users u WHERE u.account_id = a.id)
                    OR (OLD.target_kind = 'api_key'
                        AND OLD.target_id IN (SELECT k.id FROM api_keys k WHERE k.account_id = a.id))
                    OR OLD.metadata ->> 'target_identifier' = 'acct:' || a.slug
                    OR OLD.metadata ->> 'actor_identifier' = 'acct:' || a.slug))
    THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege',
              HINT = 'Permitted: the ON DELETE SET NULL of a deleted account or user (0179), '
                     'and the erasure scrub of a closed account''s rows (0188).';
END;
$$;

COMMENT ON FUNCTION audit_log_append_only() IS
    'Refuses UPDATE, DELETE and TRUNCATE of audit_log, except the ON DELETE '
    'SET NULL of account_id / actor_user_id once the referenced row is gone '
    '(0179, GH #969) and an account erasure''s scrub of metadata and, on '
    'non-staff rows, ip / user_agent (0188, GH #809).';

COMMIT;
