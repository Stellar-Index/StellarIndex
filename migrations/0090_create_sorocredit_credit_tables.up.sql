-- 0090 up — sorocredit: consumer-USDC credit / CDP protocol tables.
--
-- New protocol domain (Stellar Index had NO lending/credit-position
-- tables before this). Source: internal/sources/sorocredit — an
-- unbranded consumer-USDC credit / CDP protocol whose single main
-- contract is
--   CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR
-- (creator GADI6FHS…, wasm 84a88013…). It runs its own USDC book
-- (verified independent — not a wrapper). Raw events are already 100%
-- in the ClickHouse lake (ADR-0034); this migration adds the served-tier
-- tables the projector (ADR-0031/0032, sole writer) fills.
--
-- Four tables model the event surface faithfully (schemas verified
-- against real lake fixtures 2026-07-07):
--
--   credit_positions   ← NewCollateralContract — a position is opened; the
--                         contract deploys a per-user Collateral-<uuid>
--                         child. topic[1] = the child C-address (position
--                         identity); body = (name string, owner G-addr).
--   credit_statements  ← StatementPublished — a periodic per-position
--                         charge/settlement statement. topics carry the
--                         statement + position UUIDs; body = (amount i128,
--                         collateral C-addr, timestamp u64).
--   credit_settlements ← the on-wire "Liquidation" event. **CRITICAL: these
--                         are SCHEDULED SETTLEMENTS, NOT distressed
--                         liquidations.** A single keeper account
--                         (GA3PWX3H…) executes ALL of them, ~1:1 with
--                         StatementPublished (lake: 187,926 statements vs
--                         187,718 "Liquidation"s over the contract's life)
--                         and ~14/user/month uniformly — i.e. recurring
--                         settlements of published statements, not risk
--                         events. The table + column + source EventType are
--                         named `settlement` on purpose; NEVER surface a
--                         "221k liquidations" risk signal from this data.
--   credit_events      ← Withdrawal (position cash-out) + the three
--                         low-volume config events (BeaconUpdated,
--                         SupportedAssetAdded, CollateralHashUpdated),
--                         event_type-discriminated. Captures the remaining
--                         events faithfully per the EVERY-event invariant
--                         rather than dropping them.
--
-- i128 amounts are NUMERIC (ADR-0003 — never int64). This protocol has no
-- published price; NONE of these rows reach the trades hypertable or VWAP —
-- ADDITIVE explorer coverage, orthogonal to pricing.
--
-- Contract-identity gate (ADR-0035): the decoder claims these events ONLY
-- from the single trust-root main contract (+ its announced children) — two
-- other mainnet contracts emit the same distinctive symbols and are
-- rejected. See internal/sources/sorocredit/README.md.
--
-- Retention: NONE — the granular-coverage mission keeps credit history
-- forever.
--
-- Historical fill: live ingest writes here from deploy onward; the
-- back-window is re-derived from the lake with
--   stellarindex-ops projector-replay -source sorocredit -from 61620822
-- run under /usr/local/sbin/run-heavy-job.sh (CLAUDE.md heavy-job doctrine).
-- BackfillSafe stays false until a WASM-history audit lands.

BEGIN;

-- ─── credit_positions ───────────────────────────────────────────────
-- One row per opened position (NewCollateralContract). The position's
-- on-chain identity is the child Collateral-<uuid> contract (a per-user
-- sub-contract this event deploys); position_uuid is the UUID parsed from
-- its name so statements/settlements join by it.
CREATE TABLE credit_positions (
    -- Child Collateral-<uuid> contract C-strkey — the position identity.
    collateral_contract text         NOT NULL,
    -- UUID parsed from the "Collateral-<uuid>" child name (join key to
    -- credit_statements.position_uuid / credit_settlements.position_uuid).
    position_uuid       text         NOT NULL,
    -- Raw child-contract name ("Collateral-<uuid>") as emitted.
    position_name       text         NOT NULL,
    -- Position owner (G-strkey).
    owner               text         NOT NULL,

    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            integer      NOT NULL CHECK (op_index >= 0),
    event_index         integer      NOT NULL CHECK (event_index >= 0),

    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, collateral_contract, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE credit_positions IS
    'Opened positions in the sorocredit consumer-USDC credit protocol '
    '(NewCollateralContract). collateral_contract is the per-user '
    'Collateral-<uuid> child contract this event deploys. No published '
    'price; never contributes to VWAP. Hypertable on ledger_close_time. '
    'See internal/sources/sorocredit/README.md.';

SELECT create_hypertable('credit_positions', 'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days', if_not_exists => TRUE);

-- Position lookup by the child contract / by owner.
CREATE INDEX credit_positions_owner_ts_idx
    ON credit_positions (owner, ledger_close_time DESC);
CREATE INDEX credit_positions_uuid_idx
    ON credit_positions (position_uuid);

ALTER TABLE credit_positions SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'owner',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);
SELECT add_compression_policy('credit_positions', INTERVAL '7 days', if_not_exists => TRUE);

-- ─── credit_statements ──────────────────────────────────────────────
-- One row per published statement (StatementPublished) — a periodic
-- per-position charge/settlement statement.
CREATE TABLE credit_statements (
    statement_uuid      text         NOT NULL,
    position_uuid       text         NOT NULL,
    -- Collateral child contract the statement is against (event body).
    collateral_contract text         NOT NULL,
    -- Statement amount. NUMERIC per ADR-0003 (i128 never truncates). A
    -- statement can legitimately be 0, so the check is >= 0.
    amount              numeric      NOT NULL CHECK (amount >= 0),
    -- The statement's own timestamp (u64 unix seconds in the event body),
    -- distinct from ledger_close_time.
    statement_time      timestamptz  NOT NULL,

    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            integer      NOT NULL CHECK (op_index >= 0),
    event_index         integer      NOT NULL CHECK (event_index >= 0),

    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, statement_uuid, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE credit_statements IS
    'Periodic per-position statements in the sorocredit credit protocol '
    '(StatementPublished). No published price; never contributes to VWAP. '
    'Hypertable on ledger_close_time. See internal/sources/sorocredit/README.md.';

SELECT create_hypertable('credit_statements', 'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days', if_not_exists => TRUE);

-- Per-position statement history.
CREATE INDEX credit_statements_position_ts_idx
    ON credit_statements (position_uuid, ledger_close_time DESC);

ALTER TABLE credit_statements SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'position_uuid',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);
SELECT add_compression_policy('credit_statements', INTERVAL '7 days', if_not_exists => TRUE);

-- ─── credit_settlements ─────────────────────────────────────────────
-- One row per SCHEDULED settlement (on-wire "Liquidation"). NAMED
-- `settlement`, NOT `liquidation`: a single keeper settles published
-- statements on a schedule — these are NOT distressed liquidations (see
-- the header + the source README). debt_asset / settled_amount are the
-- primary (USDC) leg promoted from parallel body vectors; the full body
-- — every trailing protocol-internal field — is preserved in `attributes`.
CREATE TABLE credit_settlements (
    -- Collateral child contract being settled (event topic).
    collateral_contract text         NOT NULL,
    position_uuid       text         NOT NULL,
    statement_uuid      text         NOT NULL,
    -- The keeper account that executed the settlement (single recurring
    -- keeper in practice) — G-strkey.
    settler_account     text         NOT NULL,
    -- Primary debt asset (USDC SAC contract C-strkey). Nullable: promoted
    -- from a nested body Vec that degrades gracefully; the raw body is
    -- always in `attributes`.
    debt_asset          text,
    -- Primary settled amount. NUMERIC per ADR-0003. Nullable for the same
    -- reason as debt_asset. >= 0 when present.
    settled_amount      numeric      CHECK (settled_amount IS NULL OR settled_amount >= 0),
    -- Full event body (all parallel/trailing vectors) rendered for
    -- fidelity + forward-compat.
    attributes          jsonb        NOT NULL DEFAULT '{}',

    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            integer      NOT NULL CHECK (op_index >= 0),
    event_index         integer      NOT NULL CHECK (event_index >= 0),

    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, position_uuid, statement_uuid, ledger,
                 tx_hash, op_index, event_index)
);

COMMENT ON TABLE credit_settlements IS
    'SCHEDULED settlements in the sorocredit credit protocol, decoded from '
    'the on-wire "Liquidation" event. NOT distressed liquidations: a single '
    'keeper settles published statements on a recurring schedule (~1:1 with '
    'credit_statements). Do NOT surface these as a liquidation/risk signal. '
    'No published price; never contributes to VWAP. Hypertable on '
    'ledger_close_time. See internal/sources/sorocredit/README.md.';

SELECT create_hypertable('credit_settlements', 'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days', if_not_exists => TRUE);

-- Per-position + per-statement settlement lookup.
CREATE INDEX credit_settlements_position_ts_idx
    ON credit_settlements (position_uuid, ledger_close_time DESC);
CREATE INDEX credit_settlements_statement_idx
    ON credit_settlements (statement_uuid);

ALTER TABLE credit_settlements SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'position_uuid',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);
SELECT add_compression_policy('credit_settlements', INTERVAL '7 days', if_not_exists => TRUE);

-- ─── credit_events ──────────────────────────────────────────────────
-- Catch-all for the remaining events (EVERY-event invariant): Withdrawal
-- (position cash-out) + the three low-volume protocol-config events.
-- event_type-discriminated; promoted columns vary by type (NULL when the
-- type carries no such field), full body in `attributes`.
CREATE TABLE credit_events (
    event_type          text         NOT NULL CHECK (event_type IN (
                            'withdrawal', 'beacon_updated',
                            'supported_asset_added', 'collateral_hash_updated')),
    -- Withdrawal: the collateral child contract. NULL for config events.
    collateral_contract text,
    -- Withdrawal: token (USDC SAC); supported_asset_added: the asset. Else NULL.
    asset               text,
    -- Withdrawal: recipient (G-strkey). Else NULL.
    account             text,
    -- Withdrawal: amount. NUMERIC per ADR-0003. NULL for config events.
    amount              numeric      CHECK (amount IS NULL OR amount >= 0),
    attributes          jsonb        NOT NULL DEFAULT '{}',

    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            integer      NOT NULL CHECK (op_index >= 0),
    event_index         integer      NOT NULL CHECK (event_index >= 0),

    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, event_type, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE credit_events IS
    'Withdrawal + protocol-config events (beacon/supported-asset/collateral-'
    'hash) for the sorocredit credit protocol, event_type-discriminated. '
    'Faithful capture of the events outside the three structured tables '
    '(EVERY-event invariant). No published price; never contributes to VWAP. '
    'Hypertable on ledger_close_time. See internal/sources/sorocredit/README.md.';

SELECT create_hypertable('credit_events', 'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days', if_not_exists => TRUE);

-- Per-type scan ("recent withdrawals") + per-collateral lookup.
CREATE INDEX credit_events_type_ts_idx
    ON credit_events (event_type, ledger_close_time DESC);
CREATE INDEX credit_events_collateral_ts_idx
    ON credit_events (collateral_contract, ledger_close_time DESC);

ALTER TABLE credit_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'event_type',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);
SELECT add_compression_policy('credit_events', INTERVAL '7 days', if_not_exists => TRUE);

COMMIT;
