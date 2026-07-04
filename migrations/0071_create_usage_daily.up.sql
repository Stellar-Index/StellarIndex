-- 0071 up — `usage_daily` hypertable: per-endpoint API usage rollups.
--
-- Backs the dashboard's per-endpoint usage analytics
-- (/v1/account/usage). The API binary's usage-rollup worker
-- (internal/usage.Rollup) folds the Redis per-endpoint detail
-- counters (`usage:ep:<subject>:<day>` hashes, 35-day TTL) into
-- this table every 5 minutes, sweeping today + yesterday (UTC).
-- Redis stays the hot write path (per-request HINCRBY); this table
-- is the durable, queryable rollup — it outlives the Redis TTL and
-- is the only place per-endpoint history exists beyond 35 days.
--
-- One row per (day, subject, endpoint):
--   subject   — the usage counter key ("key:<KeyID>" for API-key
--               callers, "id:<Identifier>" otherwise); matches
--               middleware.usageKeyForSubject.
--   endpoint  — the mux route PATTERN (e.g. "/v1/assets/{asset_id}"),
--               never a raw URL, so cardinality is bounded by the
--               route table (~80) plus "unmatched".
--   ok_count / client_error_count / server_error_count — allowed
--               traffic split by outcome (<400 / 4xx-except-429 /
--               5xx). `requests` on the wire = the sum of these.
--   throttled_count — 429 rate-limit rejections. Deliberately NOT
--               part of the request total: throttled calls never
--               eat monthly quota (same policy as the legacy
--               per-day Redis totals MonthlyQuota reads).
--
-- Counts are plain bigint request tallies — no i128/NUMERIC needed
-- (ADR-0003 concerns token amounts, not HTTP counters).
--
-- The upsert is GREATEST()-merged (see timescale.UpsertUsageDaily):
-- the worker writes cumulative per-day counters, so replaying a
-- sweep is a no-op and a mid-day Redis flush can never regress a
-- row. Old-binary-safe (CS-099): purely additive — no existing
-- table, column, or policy is touched, so a pre-0071 binary runs
-- unchanged against a post-0071 schema.
--
-- Hypertable on `day` with 90-day chunks: the table is tiny
-- (subjects × endpoints × days), so wide chunks keep the chunk
-- count irrelevant (see the trades-chunk perf incident,
-- 2026-06-01). PK leads with `day` to satisfy TimescaleDB's TS103
-- (partitioning column must appear in every unique index).
-- No retention, no compression — rollups are the durable record.

CREATE TABLE IF NOT EXISTS usage_daily (
    day                 date        NOT NULL,
    subject             text        NOT NULL,
    endpoint            text        NOT NULL,
    ok_count            bigint      NOT NULL DEFAULT 0,
    client_error_count  bigint      NOT NULL DEFAULT 0,
    server_error_count  bigint      NOT NULL DEFAULT 0,
    throttled_count     bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, subject, endpoint)
);

SELECT create_hypertable(
    'usage_daily', 'day',
    chunk_time_interval => INTERVAL '90 days',
    if_not_exists       => TRUE
);

-- Read path is "everything for one subject over a trailing window"
-- (ReadUsageDaily); the PK's (day, subject, ...) order serves the
-- day-range scan but a subject-leading index makes the per-subject
-- lookup an index range scan instead of a filter.
CREATE INDEX IF NOT EXISTS usage_daily_subject_day_idx
    ON usage_daily (subject, day DESC);
