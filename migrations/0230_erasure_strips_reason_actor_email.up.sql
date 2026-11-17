-- 0230 up — account erasure also strips the free-text `reason` and a
-- staff actor's `actor_email` from kept audit rows.
--
-- Staff key mint/revoke and the admin account actions record the
-- operator's X-Reason header as `reason`. It is free text checked only for
-- being non-empty, so it can name the customer ("onboarding Acme, contact
-- jane@…"), and those rows are the ones an erasure scrubs (target
-- account, target key, or `target_identifier = 'acct:<slug>'`). A staff
-- customer lookup records the staff user's own address as `actor_email`;
-- when that staff user's account is erased, the scrub selects the row by
-- actor_user_id. audit_log_erase_metadata (0188, 0223) strips `email` but
-- neither of these, and the append-only trigger pins an erasure's UPDATE
-- to exactly that function's output, so both values stayed in the
-- permanent audit log. Stripping `reason` loses the justification text on
-- an erased account's rows; the action, actor key and timestamps remain.
--
-- This replaces the function with the two keys added; its signature is
-- unchanged. The staff ip and user_agent on such a row are the erased
-- person's own too, so audit_log_append_only also lets the scrub null them
-- on a staff row whose actor_user_id is one of the erased account's users;
-- a staff row acting on the account from elsewhere keeps both.
--
-- No back-fill. A row scrubbed before this migration belongs to an
-- account that no longer exists, so the trigger refuses a second scrub,
-- and a migration disabling the trigger would take an ACCESS EXCLUSIVE
-- lock on audit_log and step around its append-only guarantee. A
-- deployment with rows in erased_account_slugs may still hold such a row;
-- clearing it takes the table owner with the trigger disabled.
--
-- Old-binary-safe (rule 9): the previous binary calls the function on
-- both sides of its scrub, so it strips the wider list too. Runtime:
-- catalog only.

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
                       'target_account', 'credential_name',
                       'reason', 'actor_email']::text[] AS k) keys
$$;

COMMENT ON FUNCTION audit_log_erase_metadata(jsonb) IS
    'audit_log metadata with the subject-identifying keys removed (top level '
    'and inside before/after). The only metadata an account erasure may '
    'write (migrations 0188, 0223 and 0230).';

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
            OR (NEW.ip IS NULL
                AND (OLD.actor_kind <> 'staff'
                     OR OLD.actor_user_id IN (SELECT u.id FROM users u WHERE u.account_id = erasing::uuid))))
       AND (NEW.user_agent IS NOT DISTINCT FROM OLD.user_agent
            OR (NEW.user_agent IS NULL
                AND (OLD.actor_kind <> 'staff'
                     OR OLD.actor_user_id IN (SELECT u.id FROM users u WHERE u.account_id = erasing::uuid))))
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
                     'and the erasure scrub of a closed account''s rows (0188, 0230).';
END;
$$;

COMMENT ON FUNCTION audit_log_append_only() IS
    'Refuses UPDATE, DELETE and TRUNCATE of audit_log, except the ON DELETE '
    'SET NULL of account_id / actor_user_id once the referenced row is gone '
    '(0179, GH #969) and an account erasure''s scrub of metadata and of ip / '
    'user_agent on non-staff rows and on staff rows acted by the erased '
    'account''s own users (0188, 0230).';

COMMIT;
