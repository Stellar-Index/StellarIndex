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
-- This replaces the function with the two keys added. Its signature and
-- the trigger are unchanged.
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

COMMIT;
