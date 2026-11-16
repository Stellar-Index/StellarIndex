-- 0223 up — account erasure also strips webhook receiver hosts.
--
-- Webhook audit rows (webhook.create / update / delete / rotate_secret)
-- record the receiver's hostname as `url_host`, and an update that moves
-- the URL records `previous_url_host`. A receiver hostname usually names
-- the customer. audit_log_erase_metadata (0188) removes a fixed list of
-- keys and neither was on it, and the append-only trigger pins an
-- erasure's UPDATE to exactly that function's output, so the application
-- could not strip them either: an erased account's hostnames stayed in
-- the permanent audit log.
--
-- This replaces the function with the two keys added. Its signature and
-- the trigger are unchanged.
--
-- No back-fill. A row scrubbed before this migration belongs to an account
-- that no longer exists, so the trigger refuses a second scrub of it. The
-- reference deployment held no webhook audit row when this was written.
-- On another deployment such a row has a null account_id and still
-- carries one of the two keys; clearing it takes the table owner with the
-- trigger disabled.
--
-- Old-binary-safe (rule 9): the previous binary calls the function on
-- both sides of its scrub, so it strips the wider list too. Runtime:
-- catalog only.

BEGIN;

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
                       'url_host', 'previous_url_host']::text[] AS k) keys
$$;

COMMENT ON FUNCTION audit_log_erase_metadata(jsonb) IS
    'audit_log metadata with the subject-identifying keys removed (top level '
    'and inside before/after). The only metadata an account erasure may '
    'write (migrations 0188 and 0223).';

COMMIT;
