-- 0122 up — a DURABLE per-email failed-verify lockout for the dashboard
-- email-code sign-in, so brute-forcing the 6-digit code is bounded by
-- something a token re-mint (or a Redis flush) cannot reset
-- (audit-2026-07-23 C3-032).
--
-- What was already bounded, and what wasn't:
--
--   * `magic_link_tokens.attempts` IS durable and IS shared across the
--     email's live tokens (IncrementLoginCodeAttempts bumps every one),
--     so 5 wrong guesses retire the whole candidate set. But a NEW token
--     starts at attempts = 0 — the cap is per MINT, not per email.
--   * The only thing bounding re-mints is `auth.RedisLoginThrottle`
--     (5 sends/hour/target-email). That lives in Redis with an
--     in-process fallback: a FLUSHALL, a fail-over, or a process
--     restart resets it, and it is a fixed window that resets hourly
--     regardless.
--
--   Net standing budget: ~25 code guesses/hour/email, indefinitely,
--   against a ~1e6 code space ≈ 2.5e-5/hour ≈ 0.22 over a year of
--   patient grinding on ONE targeted address. That is not a
--   theoretical margin for an account-takeover primitive.
--
-- This table is the missing durable dimension: failures counted per
-- EMAIL, across mints, in Postgres.
--
--   failed_count      failures inside the current window.
--   window_started_at when the current counting window opened; the
--                     window is closed by expiry or by a successful
--                     login (which DELETEs the row — proof of
--                     ownership retires the suspicion).
--   locked_until      set when failed_count crosses the threshold.
--                     While in the future the code path refuses to
--                     match, without consuming candidates.
--   last_failed_at    operator forensics: how recent is the grinding.
--
-- The policy numbers (threshold, window, lockout duration) live in Go
-- (`dashboardauth.maxDurableCodeFailures` et al), not in a column
-- default: they are a property of the login flow, and a schema change
-- is the wrong tool for tuning them.
--
-- Deliberately gates the CODE path only. The magic LINK in the same
-- email keeps working, exactly as the pre-existing per-token
-- `maxCodeAttempts` cap does. That is what makes a long lockout safe:
-- a legitimate user who fat-fingers their way into one clicks the link
-- instead, while an attacker grinding codes gets no fresh budget by
-- re-minting. It also means the lockout cannot be used to lock a victim
-- OUT of their own account — only out of one of two equivalent doors.
--
-- Email is stored in plaintext, matching `magic_link_tokens.email` in
-- migration 0027 (the same address, the same table family, and the same
-- 90-day operator-forensics need). Hashing here would buy nothing while
-- the adjacent table holds the plaintext, and would stop an operator
-- clearing a lockout for a customer who calls support.
--
-- Additive: a brand-new table, so no released binary is affected
-- (migrations/README.md rule 9).

BEGIN;

CREATE TABLE IF NOT EXISTS login_code_lockouts (
    email             text PRIMARY KEY,
    failed_count      integer     NOT NULL DEFAULT 0,
    window_started_at timestamptz NOT NULL DEFAULT now(),
    last_failed_at    timestamptz NOT NULL DEFAULT now(),
    locked_until      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE login_code_lockouts IS
    'Durable per-email failed-verify counter for the dashboard 6-digit '
    'email-code sign-in (C3-032). Survives a token re-mint and a Redis flush, '
    'which is the point: magic_link_tokens.attempts resets on every new mint '
    'and the send throttle it relies on is Redis-only. Gates the CODE path '
    'only — the magic link in the same email keeps working.';

COMMENT ON COLUMN login_code_lockouts.locked_until IS
    'While in the future, POST /v1/auth/verify-code refuses to match a code '
    'for this address (and returns the same generic error as every other '
    'failure, so it is not an enumeration oracle). NULL = not locked.';

-- Operator view: who is being ground on right now.
CREATE INDEX IF NOT EXISTS login_code_lockouts_locked_idx
    ON login_code_lockouts (locked_until)
    WHERE locked_until IS NOT NULL;

-- Retention sweep index. The row key is an ATTACKER-CHOSEN string: the
-- verify-code endpoint is unauthenticated and takes an arbitrary
-- well-formed address, so one wrong guess against a synthetic address
-- inserts a row that no successful sign-in will ever delete (nobody owns
-- it). Bounded only by the anonymous rate limit, that is unbounded table
-- growth on a disk-fixed host — a slow, cheap, remote fill.
--
-- `internal/logincodereaper` therefore deletes settled rows on a
-- schedule:
--
--   DELETE FROM login_code_lockouts
--    WHERE updated_at < now() - <retention>
--      AND (locked_until IS NULL OR locked_until <= now());
--
-- The second predicate is what makes this safe: a LIVE lock is never
-- reaped regardless of age. And reaping a settled row loses nothing —
-- retention (48 h) is longer than the counting window (24 h), so any row
-- old enough to be swept is already a fresh window by policy.
--
-- The index leads with updated_at to match that predicate's driving
-- column; the sweep is a range scan, not a seq scan over a table an
-- attacker controls the size of.
CREATE INDEX IF NOT EXISTS login_code_lockouts_updated_at_idx
    ON login_code_lockouts (updated_at);

COMMIT;
