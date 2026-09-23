-- 0167 up — bound `usage_daily` (0071) to 12 months.
--
-- 0071 shipped the table with "No retention ... rollups are the
-- durable record". Its `subject` is the owner-account key
-- ("id:acct:<slug>"), so every row is one customer's per-endpoint
-- request history, and nothing ever removed it: the Redis source
-- expires at 35 days, the table kept everything forever.
--
-- 12 months is the horizon this platform already set for per-request
-- usage data: `api_usage_events` carries the same policy (0027) and
-- docs/architecture/platform-spec.md §3.1 specifies it. Nothing in
-- the product reads further back — /v1/account/usage serves 30 days.
--
-- Armed on apply, unlike 0156: the oldest possible row dates from
-- 0071's rollout (2026-07), so the first chunk to age out is roughly
-- a year away and applying this drops nothing today. Chunks are 90
-- days wide (0071), so a row lives between 12 and ~15 months.
--
-- Also corrects 0071's header, which is immutable: its "same policy
-- as the legacy per-day Redis totals" line is wrong — the legacy
-- total excludes 5xx as well as 429. The quota-counted units are
-- ok_count + client_error_count (served as `billable`).

BEGIN;

SELECT add_retention_policy(
         'usage_daily',
         drop_after    => INTERVAL '12 months',
         if_not_exists => true);

COMMIT;
