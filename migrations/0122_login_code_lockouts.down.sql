-- 0122 down — drop the durable login-code lockout.
--
-- Reverting re-opens C3-032: the only remaining bound on 6-digit code
-- guessing becomes `magic_link_tokens.attempts` (which a re-mint resets)
-- plus the Redis-only send throttle (which a flush, a fail-over or a
-- restart resets). That returns the standing budget to ~25 guesses per
-- hour per targeted email, indefinitely.
--
-- Export any open lockouts first if an incident is in progress — the
-- rows are the evidence of who was being ground on:
--
--   SELECT email, failed_count, window_started_at, last_failed_at, locked_until
--     FROM login_code_lockouts
--    WHERE locked_until IS NOT NULL AND locked_until > now();
--
-- The post-0122 binary reads and writes this table on the verify-code
-- path, so reverting must be paired with a rollback to a pre-0122 binary
-- (migrations/README.md rule 9's two-release dance). Reverting the
-- migration alone makes every code sign-in attempt error on a missing
-- relation.

BEGIN;

DROP INDEX IF EXISTS login_code_lockouts_updated_at_idx;
DROP INDEX IF EXISTS login_code_lockouts_locked_idx;
DROP TABLE IF EXISTS login_code_lockouts;

COMMIT;
