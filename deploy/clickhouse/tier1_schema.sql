-- Tier-1 raw lake schema (ADR-0034 / docs/architecture/clickhouse-migration-plan.md §5).
-- Structural, decoder-INDEPENDENT decode of every ledger; raw XDR blobs retained
-- so any protocol decoder (event / op / contract-call / ledger-entry-change) can
-- run from ClickHouse without re-touching galexie.
--
-- Engine: ReplacingMergeTree(ingested_at) -> idempotent re-ingest (latest wins on
-- merge; NO ON CONFLICT silent-drop like the Postgres soroban_events bug). Query
-- with FINAL / GROUP BY for read-time dedup until merges settle.
-- Partitioned by 1M-ledger ranges; ORDER BY = each row's natural unique identity.

CREATE DATABASE IF NOT EXISTS stellar;

-- One row per ledger (also serves the ADR-0033 substrate/census role).
CREATE TABLE IF NOT EXISTS stellar.ledgers
(
    ledger_seq                 UInt32,
    close_time                 DateTime('UTC'),
    ledger_hash                String,
    prev_hash                  String,
    protocol_version           UInt32,
    bucket_list_hash           String,
    tx_count                   UInt32,
    op_count                   UInt32,
    soroban_event_count        UInt32,
    classic_trade_effect_count UInt32,
    total_coins                Int64,
    fee_pool                   Int64,
    base_fee                   UInt32,
    base_reserve               UInt32,
    ingested_at                DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY ledger_seq;

CREATE TABLE IF NOT EXISTS stellar.transactions
(
    ledger_seq      UInt32,
    close_time      DateTime('UTC'),
    tx_hash         String,
    tx_index        UInt32,
    source_account  String,
    fee_charged     Int64,
    max_fee         Int64,
    operation_count UInt16,
    successful      UInt8,
    result_code     Int32,
    memo_type       LowCardinality(String),
    memo            String,
    ingested_at     DateTime DEFAULT now(),
    -- Bloom skip-index for hash lookups (GET /v1/tx/{hash}, ADR-0038): the
    -- sort key is (ledger_seq, tx_index), so WHERE tx_hash=? would otherwise
    -- full-scan. New parts are indexed on insert; existing history needs a
    -- one-time `ALTER TABLE stellar.transactions MATERIALIZE INDEX idx_tx_hash`.
    INDEX idx_tx_hash tx_hash TYPE bloom_filter(0.01) GRANULARITY 1,
    -- Per-account submitted-tx lookups (GET /v1/accounts/{g}/transactions).
    INDEX idx_tx_source source_account TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_index);

-- body_xdr (base64) lets any OpDecoder (SDEX claim-atoms, Rozo classic payments,
-- change_trust, …) run from ClickHouse.
CREATE TABLE IF NOT EXISTS stellar.operations
(
    ledger_seq     UInt32,
    close_time     DateTime('UTC'),
    tx_hash        String,
    tx_index       UInt32,
    op_index       UInt32,
    op_type        LowCardinality(String),
    source_account String,
    body_xdr       String,
    ingested_at    DateTime DEFAULT now(),
    -- Per-account sourced-operation lookups (GET /v1/accounts/{g}/operations);
    -- sort key is (ledger_seq, tx_index, op_index) so a source_account
    -- predicate would otherwise full-scan. MATERIALIZE INDEX for history.
    INDEX idx_op_source source_account TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_index, op_index);

-- Per-op results — SDEX claim atoms, path-payment fills.
CREATE TABLE IF NOT EXISTS stellar.operation_results
(
    ledger_seq  UInt32,
    tx_hash     String,
    op_index    UInt32,
    result_code Int32,
    result_xdr  String,
    ingested_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index);

-- The soroban_events replacement. Retains topic/body/arg XDR for any event decoder.
CREATE TABLE IF NOT EXISTS stellar.contract_events
(
    ledger_seq         UInt32,
    close_time         DateTime('UTC'),
    tx_hash            String,
    op_index           UInt32,
    event_index        UInt32,
    contract_id        String,
    event_type         LowCardinality(String),
    topic_count        UInt8,
    topic_0_sym        String,
    topics_xdr         Array(String),
    data_xdr           String,
    op_args_xdr        Array(String),
    in_successful_call UInt8,
    ingested_at        DateTime DEFAULT now(),
    -- Bloom skip-index for per-contract activity (GET /v1/contracts/{c},
    -- ADR-0038): the sort key is (ledger_seq, tx_hash, ...), so WHERE
    -- contract_id=? would otherwise full-scan. New parts indexed on insert;
    -- existing history needs `ALTER TABLE stellar.contract_events
    -- MATERIALIZE INDEX idx_contract_id`.
    INDEX idx_contract_id contract_id TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index, event_index);

-- State deltas — supply/account/trustline/offer/contract-data observers.
-- op_index = -1 for fee-meta / tx-level changes.
CREATE TABLE IF NOT EXISTS stellar.ledger_entry_changes
(
    ledger_seq   UInt32,
    close_time   DateTime('UTC'),
    tx_hash      String,
    op_index     Int32,
    change_index UInt32,
    change_type  LowCardinality(String),
    entry_type   LowCardinality(String),
    key_xdr      String,
    entry_xdr    String,
    -- Queryable owner + asset (ADR-0038 Phase C account-state / asset-holder
    -- reads). account_id = owning G-strkey for account-owned entries (account
    -- / trustline / offer / data); asset = canonical "CODE-ISSUER" / "native"
    -- / "pool:<hex>" for trustlines. Empty otherwise. Bloom skip-indexes so a
    -- WHERE account_id=? / asset=? prunes parts — the sort key is
    -- (ledger_seq, tx_hash, …), so these predicates would otherwise full-scan.
    -- Existing rows backfill to '' until a ch re-derive repopulates them.
    account_id   String DEFAULT '',
    asset        String DEFAULT '',
    ingested_at  DateTime DEFAULT now(),
    INDEX idx_lec_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lec_asset asset TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index, change_index);

-- Per-token supply events (CAP-67 classic SAC + SEP-41 mint/burn/clawback) with
-- the i128 amount DECODED at ingest (decode-at-ingest, ADR-0034). Total supply
-- for a token is a pure SQL sum over this table:
--   Σ amount WHERE kind='mint' − Σ amount WHERE kind IN ('burn','clawback')
-- — no XDR decode at read time and no periodic rollup refresh (the dual-sink
-- keeps it real-time; ch-backfill re-fills holes). ORDER BY contract_id first
-- so a per-token read is a fast PK-prefix scan; the (ledger,tx,op,event) suffix
-- is the event identity, so re-ingest (drop→heal / re-backfill) is idempotent.
CREATE TABLE IF NOT EXISTS stellar.supply_flows
(
    contract_id  String,
    ledger_seq   UInt32,
    close_time   DateTime('UTC'),
    tx_hash      String,
    op_index     UInt32,
    event_index  UInt32,
    kind         LowCardinality(String),
    amount       Int128,
    ingested_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (contract_id, ledger_seq, tx_hash, op_index, event_index);
