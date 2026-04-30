-- 0015 up — `sep41_supply_events` hypertable.
--
-- Per ADR-0023 + ADR-0011 Algorithm 3. Stores one row per
-- mint / burn / clawback event observed on a watched SEP-41
-- contract. The running sum
-- `Σ mint − Σ(burn + clawback)` over the contract's lifetime
-- yields total_supply per ADR-0011 Algorithm 3.
--
-- Identity: (contract_id, ledger, tx_hash, op_index, observed_at).
-- Same Soroban-event shape the trades + blend_auctions hypertables
-- use. Per docs/audit-2026-04-29 F-0010, observed_at drags into
-- the PK because Timescale requires the partition column there;
-- the natural identity is the (contract_id, ledger, tx_hash,
-- op_index) tuple.
--
-- Watched-set restricted: only events from operator-configured
-- contracts in `[supply] watched_sep41_contracts` reach this
-- table. Network-wide SEP-41 event volume is high (transfers
-- dominate) but mint/burn/clawback subsets are small per
-- contract — this table grows slowly.
--
-- Retention: NONE for now. The running sum is the entire reason
-- the table exists, so we never want to drop history. Re-running
-- the sum from a cold state requires every row.

BEGIN;

CREATE TABLE sep41_supply_events (
    -- SEP-41 contract C-strkey.
    contract_id   text         NOT NULL,

    -- Soroban event identity.
    ledger        integer      NOT NULL CHECK (ledger >= 0),
    tx_hash       char(64)     NOT NULL,
    op_index      integer      NOT NULL CHECK (op_index >= 0),
    observed_at   timestamptz  NOT NULL,

    -- Event variant. The CHECK matches what
    -- internal/canonical/discovery.classifySymbol returns for
    -- SEP-41 supply events. `transfer` is intentionally excluded
    -- — transfers move ownership, not supply.
    event_kind    text         NOT NULL CHECK (event_kind IN ('mint', 'burn', 'clawback')),

    -- The amount, NUMERIC per ADR-0003. Always non-negative;
    -- event_kind discriminates direction (mint adds, burn /
    -- clawback subtract).
    amount        numeric      NOT NULL CHECK (amount >= 0),

    -- The counterparty:
    --   - mint: recipient (`to`)
    --   - burn: holder being burned (`from`)
    --   - clawback: holder being clawed back (`from`)
    -- NULL is reserved for future event variants without a
    -- counterparty (none today).
    counterparty  text,

    ingested_at   timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, observed_at)
);

COMMENT ON TABLE sep41_supply_events IS
    'Per-event mint/burn/clawback rows for watched SEP-41 contracts. '
    'Backs Algorithm 3 supply via Σ mint − Σ(burn + clawback) per '
    'contract per ADR-0023. Hypertable on observed_at.';

COMMENT ON COLUMN sep41_supply_events.amount IS
    'Always non-negative; event_kind discriminates direction.';
COMMENT ON COLUMN sep41_supply_events.counterparty IS
    'Recipient (mint) or holder (burn/clawback). NULL reserved.';

SELECT create_hypertable(
    'sep41_supply_events',
    'observed_at',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Most-common reader query: "running sum for this contract at-or-
-- before ledger N." (contract_id, ledger DESC) covers the WHERE +
-- the ORDER BY without a sort.
CREATE INDEX sep41_supply_events_contract_ledger_idx
    ON sep41_supply_events (contract_id, ledger DESC);

-- Replay / debug — walk by ledger across all watched contracts.
CREATE INDEX sep41_supply_events_ledger_idx
    ON sep41_supply_events (ledger DESC);

ALTER TABLE sep41_supply_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id',
    timescaledb.compress_orderby   = 'observed_at DESC, ledger DESC'
);

COMMIT;
