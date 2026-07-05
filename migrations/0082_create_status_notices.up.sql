-- 0082 up — `status_notices`: operator-posted customer-facing status banners.
--
-- The "incident tooling" half of the admin Phase 1.5 surface
-- (platform-spec §7.1 "Trigger maintenance mode banner on customer
-- dashboard"). Distinct from the two existing incident surfaces:
--
--   - `/v1/incidents` serves the embedded post-mortem corpus
--     (`internal/incidents/data/*.md`) — historical, build-time,
--     read-only. There is deliberately no runtime write path there.
--   - `/v1/status`'s `incidents` block is derived from Alertmanager
--     (auto-firing alerts) — machine-observed, not human-authored.
--
-- This table is the third, missing shape: a LIVE, human-authored
-- banner an operator posts during a maintenance window or an
-- unfolding incident, and clears when it's over. It is written ONLY
-- by the operator-tier `/v1/admin/status-notices` endpoints (every
-- mutation audit-logged) and read by the public `/v1/status/notices`
-- endpoint the status page renders.
--
-- Lifecycle: a notice is `active` on create; an operator flips it to
-- `resolved` (stamping `resolved_at`) when the situation clears. Rows
-- are never deleted — resolved notices stay for the operator history
-- view + the audit trail.
--
-- severity ladder mirrors the customer-facing SEV convention but is
-- banner-scoped (`maintenance` for planned windows; minor/major/
-- critical for unplanned). No i128/NUMERIC here — this is text +
-- timestamps, not on-chain amounts (ADR-0003 is about token values).
--
-- Old-binary-safe (CS-099): purely additive — a new standalone
-- table, no existing table/column/policy touched, so a pre-0082
-- binary runs unchanged against a post-0082 schema.
CREATE TABLE status_notices (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title       text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    body        text NOT NULL CHECK (length(body) BETWEEN 1 AND 5000),
    severity    text NOT NULL
                  CHECK (severity IN ('maintenance', 'minor', 'major', 'critical')),
    status      text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'resolved')),
    -- Free-form operator reference (the acting credential's identifier
    -- / key id) for a quick "who posted this" without joining audit_log.
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);

-- The public read path lists only active notices, newest first —
-- a partial index keeps that query index-only regardless of how
-- much resolved history accumulates.
CREATE INDEX status_notices_active_idx
    ON status_notices (created_at DESC)
    WHERE status = 'active';
