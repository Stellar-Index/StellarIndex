-- 0188 up — let an account erasure scrub its own audit rows, and remember
-- erased slugs.
--
-- WHAT IS WRONG TODAY
--
-- A customer can ask for their account to be erased (GH #809). The rows
-- that hold them are deleted, but audit_log keeps the customer's IP and
-- user agent on every row they wrote, and several writers put the account
-- slug (derived from the email local part), key labels, passkey names and
-- suspension reasons in `metadata`. 0179 refuses every UPDATE other than
-- the ON DELETE SET NULL action, so none of that can be removed, and
-- audit_log has no retention job: it would be kept forever.
--
-- Separately, a slug freed by a delete is reusable, and the Redis API-key
-- records, usage counters and validator status cache are keyed by
-- `acct:<slug>`. slugFromEmail uses the local part only, so a DIFFERENT
-- person (john@a.example after john@b.example) would get the old slug and
-- could inherit anything a cleanup missed.
--
-- WHAT THIS MIGRATION ADDS
--
--   audit_log_erase_metadata(jsonb)  removes the subject-identifying keys
--       (account_slug, target_identifier, actor_identifier, label, name,
--       suspended_reason, email) from a metadata object, at the top level
--       and inside `before` / `after`. Deterministic, so the trigger can
--       require exactly its result.
--   audit_log_append_only()          0179's function plus ONE more
--       permitted UPDATE, below. DELETE and TRUNCATE stay refused.
--   erased_account_slugs             sha256 of every erased slug; account
--       creation treats a match as a slug collision.
--
-- THE ERASURE UPDATE. Allowed only when ALL hold:
--   * the transaction set `stellarindex.erasing_account` (SET LOCAL) to an
--     account id, and that account exists with status 'closed';
--   * the row belongs to that account: account_id, an 'account' target,
--     an actor user of the account, an 'api_key' target among its keys, or
--     a target/actor identifier of `acct:<its slug>`;
--   * no column changes except metadata, ip and user_agent;
--   * metadata becomes exactly audit_log_erase_metadata(OLD.metadata);
--   * ip and user_agent are unchanged, or set to NULL on a row whose actor
--     is not staff. A staff row keeps the staff member's own address and
--     agent: that is what 0179 (GH #969) protects.
--
-- WHAT THIS DOES NOT PROTECT AGAINST. The GUC is settable by any session
-- of the app role, and the app role can close an account. A statement
-- with arbitrary DML can therefore close an account and then scrub its
-- rows — never delete them, never touch staff addresses or any other
-- column. As with 0179, the trigger stops application bugs, not a session
-- that can run ALTER TABLE ... DISABLE TRIGGER. r1 held 0 audit rows when
-- this was written.
--
-- THE TOMBSTONE. Unsalted sha256 of a low-entropy slug is pseudonymous,
-- not anonymous: it lets the service recognise a slug it has erased, and
-- someone holding the table could confirm a guessed slug. It is kept
-- because the alternative, reissuing the slug, is the worse leak.
--
-- Rule-9 safe: the function only widens what is permitted and the table
-- is new; the previous binary never UPDATEs audit_log and never reads
-- erased_account_slugs. Runtime: catalog only plus one empty table.

BEGIN;

CREATE FUNCTION audit_log_erase_metadata(m jsonb) RETURNS jsonb
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
                       'label', 'name', 'suspended_reason', 'email']::text[] AS k) keys
$$;

COMMENT ON FUNCTION audit_log_erase_metadata(jsonb) IS
    'audit_log metadata with the subject-identifying keys removed (top level '
    'and inside before/after). The only metadata an account erasure may '
    'write (migration 0188, GH #809).';

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

CREATE TABLE erased_account_slugs (
    slug_sha256 bytea PRIMARY KEY CHECK (length(slug_sha256) = 32),
    erased_at   timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE erased_account_slugs IS
    'sha256(slug) of every erased account. Account creation treats a match '
    'as a slug collision, so no later account authenticates as, or inherits '
    'Redis state keyed by, acct:<slug> (migration 0188, GH #809).';

COMMIT;
