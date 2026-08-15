-- 0143 down — reverse of 0143 up (drop the session token-hash column +
-- its unique index).
--
-- Rollback-only, dev/local iteration lever (migrations/README.md rule 9:
-- down.sql is NOT a production rollback path — the deploy pipeline never
-- auto-runs `migrate down`). Dropping token_hash reverts sessions to the
-- raw-PK bearer scheme, which is exactly the W1-auth-passkey-2 defect, so
-- this must never run in production.

DROP INDEX IF EXISTS sessions_token_hash_idx;
ALTER TABLE sessions DROP COLUMN IF EXISTS token_hash;
