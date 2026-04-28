-- 0006 up — `discovered_assets` table for SEP-41 auto-discovery.
--
-- Every contract emitting a SEP-41 transfer / mint / burn / clawback
-- event lands one row here, keyed by contract_id. Subsequent events
-- bump event_count; first_seen_* fields preserve the original sighting.
--
-- Auto-detection lets the engine track new tokens without a code
-- change. Operators query this table to:
--
--   * audit recent contract arrivals (rate-of-arrival informs
--     whether the network is busy or suspicious activity is
--     happening);
--   * bootstrap downstream wiring (supply tracking, decoder
--     registration, asset-detail metadata fetch).
--
-- NOT a hypertable — discovered_assets is small (one row per known
-- SEP-41 contract; pubnet has hundreds, not millions) and we
-- want random-access reads on contract_id, not time-series scans.

BEGIN;

CREATE TABLE discovered_assets (
    contract_id        TEXT        PRIMARY KEY,
    first_seen_at      TIMESTAMPTZ NOT NULL,
    first_seen_ledger  BIGINT      NOT NULL CHECK (first_seen_ledger > 0),
    first_seen_event   TEXT        NOT NULL CHECK (first_seen_event IN ('transfer', 'mint', 'burn', 'clawback')),
    last_seen_at       TIMESTAMPTZ NOT NULL,
    last_seen_ledger   BIGINT      NOT NULL CHECK (last_seen_ledger > 0),
    event_count        BIGINT      NOT NULL DEFAULT 1 CHECK (event_count >= 1)
);

COMMENT ON TABLE discovered_assets IS
    'SEP-41 contracts auto-detected from the event stream. One row per contract; first_seen_* preserved on conflict, last_seen_* + event_count update.';

COMMENT ON COLUMN discovered_assets.first_seen_event IS
    'Which SEP-41 event topic first surfaced this contract (transfer | mint | burn | clawback).';

COMMENT ON COLUMN discovered_assets.event_count IS
    'How many SEP-41 events have been observed from this contract since discovery. Cheap monotonic counter.';

-- Reverse-time index for "what's new" queries — operators routinely
-- ask "which contracts appeared in the last N hours" to spot
-- suspicious deployment bursts.
CREATE INDEX discovered_assets_first_seen_idx
    ON discovered_assets (first_seen_at DESC);

COMMIT;
