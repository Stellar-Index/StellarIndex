-- 0143 up — store dashboard sessions keyed by a HASH of a random
-- cookie token, not by the raw primary key (audit-2026-08-14
-- W1-auth-passkey-2).
--
-- The defect: the session cookie carried sessions.id verbatim and the
-- middleware looked the row up by that same PK, so sessions were the ONE
-- credential stored UNHASHED (api_keys and magic_link_tokens both store
-- only sha256 of their secret). Any leak of the sessions rows — an
-- off-box backup, a read-replica exposure, a support export, or a single
-- future log line that includes session.id — handed an attacker a
-- directly-replayable 30-day bearer cookie.
--
-- The fix: add a nullable `token_hash bytea`. The new binary puts a
-- high-entropy random token in the cookie and stores only sha256(token)
-- here; resolveSession hashes the incoming cookie and looks up by hash.
-- A read of this table is no longer directly replayable.
--
-- CUTOVER: this is additive only. Existing rows keep token_hash = NULL;
-- under the new binary they match no cookie hash and therefore no longer
-- resolve, so every currently-active session is invalidated exactly once
-- and its user re-logs-in — the standard, accepted cost of a
-- session-secret rotation. No data is destroyed and no session is
-- silently reassigned.
--
-- OLD-BINARY-SAFE (README rule 9): ADD COLUMN nullable + CREATE UNIQUE
-- INDEX are both additive/loosening — the currently-released binary
-- neither writes nor reads token_hash, so its INSERTs (which omit the
-- column) simply leave it NULL and keep working against this schema.
-- The unique index uses the default NULLS DISTINCT, so any number of
-- old-binary rows with NULL token_hash coexist without collision.

ALTER TABLE sessions ADD COLUMN token_hash bytea;

-- One session per token; the hash is the authentication-path lookup key.
-- Partial (WHERE token_hash IS NOT NULL) so the legacy NULL rows are
-- excluded entirely rather than relying on NULLS-DISTINCT semantics.
CREATE UNIQUE INDEX sessions_token_hash_idx ON sessions (token_hash)
    WHERE token_hash IS NOT NULL;
