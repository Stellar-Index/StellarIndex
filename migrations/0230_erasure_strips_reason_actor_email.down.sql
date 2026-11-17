-- 0230 down — restore 0223's audit_log_erase_metadata verbatim. Local/dev
-- iteration only (README rule 9): rows an erasure scrubbed while 0230 was
-- applied keep the wider scrub.

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

COMMIT;
