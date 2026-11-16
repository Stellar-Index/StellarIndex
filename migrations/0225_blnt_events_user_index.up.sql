-- 0225 up — user-first index on blnt_events.
--
-- The per-account position fold (/v1/accounts/{g}/positions) filters on
-- user_address first; none of 0224's indexes lead with it. Partial, because
-- admin, emission and factory events carry no user.
--
-- The table is weeks old and small, so the in-transaction build is brief;
-- lock_timeout makes a contended deploy fail fast instead of queueing writes.

BEGIN;

SET LOCAL lock_timeout = '5s';

CREATE INDEX IF NOT EXISTS blnt_events_user_ts_idx
    ON blnt_events (user_address, ledger_close_time DESC)
    WHERE user_address IS NOT NULL;

COMMIT;
