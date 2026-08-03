-- 0132 up — `phoenix_admin_events` hypertable (DEFENSIVE).
--
-- Phoenix pool contracts emit four admin-rotation governance events
-- (pool/src/contract.rs:784-836) that classify() names (actionAdmin)
-- but the decoder dropped (case actionAdmin → nil). This table + the
-- decoder arm close that every-event gap.
--
-- ZERO occurrences on mainnet to date (lake count 2026-08-03, ledger
-- 50M–tip: 0) — no admin rotation has happened yet. Built defensively
-- so the FIRST rotation lands rather than being silently dropped.
--
-- Wire shape (per contract source; no lake sample exists to verify):
--   topic[0] = String("XYK Pool: ")
--   topic[1] = String(<rotation phrase>)  → admin_action slug
--   body     = Address(admin)             → admin (tolerated absent)
--
-- The four phrases and their slugs:
--   "Admin replacement requested by old admin: " → replace_requested
--   "Replace with new admin: "                   → replace_set
--   "Undo admin change: "                        → undo
--   "Accepted new admin: "                        → accepted
--
-- Storage shape: per-protocol table, pool-gated (ADR-0035). No amounts,
-- no VWAP. derive_generation is the INV-3 corrective-upsert column (0110).
--
-- Retention: NONE — granular-coverage mission keeps the history forever.
--
-- Historical fill: `stellarindex-ops projector-replay -source phoenix`.

BEGIN;

CREATE TABLE phoenix_admin_events (
    -- Emitting pool contract C-strkey.
    pool               text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           smallint     NOT NULL CHECK (op_index >= 0),
    event_index        smallint     NOT NULL CHECK (event_index >= 0),

    -- Which admin-rotation step this row represents.
    admin_action       text         NOT NULL CHECK (admin_action IN (
        'replace_requested', 'replace_set', 'undo', 'accepted')),

    -- The admin address carried in the event body, when present. NULL if
    -- the body carried no address (tolerated — no lake sample confirms
    -- the exact shape).
    admin              text,

    derive_generation  integer      NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103). event_index
    -- keeps multiple rotation steps in one op distinct.
    PRIMARY KEY (ledger_close_time, pool, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE phoenix_admin_events IS
    'Phoenix pool admin-rotation governance events (replace_requested / '
    'replace_set / undo / accepted). No published price; never '
    'contributes to VWAP. 0 occurrences to date — built defensively. '
    'Hypertable on ledger_close_time. See internal/sources/phoenix/decode.go.';

SELECT create_hypertable(
    'phoenix_admin_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's admin-rotation history").
CREATE INDEX phoenix_admin_events_pool_ts_idx
    ON phoenix_admin_events (pool, ledger_close_time DESC);

ALTER TABLE phoenix_admin_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pool',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'phoenix_admin_events',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
