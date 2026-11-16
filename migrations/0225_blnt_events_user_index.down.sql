-- Revert 0225: drop the user-first index on blnt_events.
DROP INDEX IF EXISTS blnt_events_user_ts_idx;
