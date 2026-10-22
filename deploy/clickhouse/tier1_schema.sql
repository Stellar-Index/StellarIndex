-- si-apply-scope: fresh-host
--
-- The founding DDL, and the only file a fresh provision applies. Every
-- other deploy/clickhouse/*.sql is `si-apply-scope: operator` and is run by
-- hand against an EXISTING deployment; each of the objects those files
-- declare is declared here too, identically, which is what makes applying
-- this file alone sufficient for a fresh host.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- Tier-1 raw lake schema (ADR-0034 / docs/architecture/storage-considerations.md §Tier-1 lake schema).
-- Structural, decoder-INDEPENDENT decode of every ledger; raw XDR blobs retained
-- so any protocol decoder can run from ClickHouse without re-touching galexie.
--
-- Engine: ReplacingMergeTree(ingested_at) -> idempotent re-ingest (latest wins on
-- merge). Query with FINAL / GROUP BY for read-time dedup until merges settle.
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
    -- Soroban resource metering. DEFAULT 0 keeps it additive and old-binary-safe;
    -- the columns are sparse (only Soroban txs are non-zero). DECLARED = the
    -- submitter's resource bid; the three *_fee values are what core CHARGED.
    -- Pubnet meta carries no actual-instructions-consumed value, so none is
    -- stored. Go-forward only; historical Soroban txs read 0.
    soroban_instructions      UInt32 DEFAULT 0,  -- declared CPU instruction bid
    soroban_disk_read_bytes   UInt32 DEFAULT 0,  -- declared
    soroban_write_bytes       UInt32 DEFAULT 0,  -- declared
    soroban_read_entries      UInt16 DEFAULT 0,  -- declared len(footprint.ReadOnly)
    soroban_write_entries     UInt16 DEFAULT 0,  -- declared len(footprint.ReadWrite)
    soroban_resource_fee_bid  Int64  DEFAULT 0,  -- declared total resource-fee bid
    soroban_nonrefundable_fee Int64  DEFAULT 0,  -- actual non-refundable fee charged
    soroban_refundable_fee    Int64  DEFAULT 0,  -- actual refundable fee charged
    soroban_rent_fee          Int64  DEFAULT 0,  -- actual rent fee charged
    -- Fee-bump outer layer. On a fee bump tx_hash is the OUTER hash while
    -- source_account and max_fee are the INNER tx's, so these carry what the
    -- envelope adds. ''/0 on a non-fee-bump tx. Go-forward only
    -- (deploy/clickhouse/transactions_fee_bump.sql): a historical fee bump reads
    -- '' until its range is re-derived.
    inner_tx_hash             String DEFAULT '', -- inner tx hash (also indexed in tx_hash_index)
    fee_account               String DEFAULT '', -- fee payer
    fee_bump_fee              Int64  DEFAULT 0,  -- fee payer's max-fee bid (bounds fee_charged)
    inner_result_code         Int32  DEFAULT 0,  -- inner TransactionResultCode (the inner failure reason)
    -- Bloom skip-index for hash lookups (GET /v1/tx/{hash}, ADR-0038): the sort key
    -- is (ledger_seq, tx_index), so WHERE tx_hash=? would otherwise full-scan. An
    -- existing host takes it (and idx_tx_source) from
    -- deploy/clickhouse/tier1_skip_indexes.sql.
    INDEX idx_tx_hash tx_hash TYPE bloom_filter(0.01) GRANULARITY 1,
    -- Per-account submitted-tx lookups (GET /v1/accounts/{g}/transactions).
    INDEX idx_tx_source source_account TYPE bloom_filter(0.01) GRANULARITY 1,
    -- Inner-hash lookups on the bloom-scan fallback (index unavailable).
    INDEX idx_tx_inner_hash inner_tx_hash TYPE bloom_filter(0.01) GRANULARITY 1
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
    body_xdr       String CODEC(ZSTD(3)),
    ingested_at    DateTime DEFAULT now(),
    -- Per-account sourced-operation lookups (GET /v1/accounts/{g}/operations); the
    -- sort key would otherwise full-scan. Existing host:
    -- deploy/clickhouse/tier1_skip_indexes.sql.
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
    result_xdr  String CODEC(ZSTD(3)),
    ingested_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index);

-- Per-op NON-SOURCE participants (ADR-0038 Phase B): one row per (account,
-- operation) where `account` is a G-strkey the op touches but did not source
-- (payment destination, trustor, merge target, clawback victim, ...). Account
-- history (GET /v1/accounts/{g}/operations|transactions) is the UNION of
-- operations.source_account = G and a lookup here.
--
-- ORDER BY (account, ...) makes a per-account lookup a primary-key range scan, so
-- no skip-index is needed (operations.source_account is a non-prefix column and
-- carries a bloom index). Live ingest fills this go-forward; the historical
-- re-derive is a multi-day, operator-gated ch-backfill.
CREATE TABLE IF NOT EXISTS stellar.operation_participants
(
    account     String,
    ledger_seq  UInt32,
    close_time  DateTime('UTC'),
    tx_hash     String,
    tx_index    UInt32,
    op_index    UInt32,
    ingested_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (account, ledger_seq, tx_index, op_index);

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
    topics_xdr         Array(String) CODEC(ZSTD(3)),
    data_xdr           String CODEC(ZSTD(3)),
    op_args_xdr        Array(String) CODEC(ZSTD(3)),
    in_successful_call UInt8,
    ingested_at        DateTime DEFAULT now(),
    -- Bloom skip-index for per-contract activity (GET /v1/contracts/{c}, ADR-0038):
    -- the sort key is (ledger_seq, tx_hash, ...), so WHERE contract_id=? would
    -- otherwise full-scan. Existing host: deploy/clickhouse/tier1_skip_indexes.sql.
    INDEX idx_contract_id contract_id TYPE bloom_filter(0.01) GRANULARITY 1,
    -- Day-window pruning for the census rollup (ch-census-rollup): it filters on
    -- close_time every 30 min, but close_time is in neither the partition nor the
    -- sort key, and ClickHouse keeps part-level min/max only for partition-key
    -- columns. close_time is monotone in ledger_seq, so a per-granule minmax prunes
    -- a one-day window. Existing host: deploy/clickhouse/tier1_skip_indexes.sql
    -- (ADD INDEX, then per-partition MATERIALIZE INDEX).
    INDEX idx_ce_close_time close_time TYPE minmax GRANULARITY 1
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
    -- CODEC(ZSTD(3)) must match the live ALTER (measured 1.75x over the LZ4
    -- default on entry_xdr).
    key_xdr      String CODEC(ZSTD(3)),
    entry_xdr    String CODEC(ZSTD(3)),
    -- ingested_at sits HERE, before the ADR-0038 columns below, because that is the
    -- live column order: those were ALTER TABLE ADDed after it, and the drift check
    -- compares ordered column lists.
    ingested_at  DateTime DEFAULT now(),
    -- Queryable owner + asset (ADR-0038 Phase C). account_id = owning G-strkey for
    -- account-owned entries (account / trustline / offer / data); asset = canonical
    -- "CODE-ISSUER" / "native" / "pool:<hex>" for trustlines. Empty otherwise.
    -- account_id carries a bloom skip-index since the sort key is (ledger_seq,
    -- tx_hash, ...). asset has none: asset-holder reads use ledger_entries_current's
    -- idx_lecur_asset; WHERE asset=? here scans every part.
    -- Existing rows read '' until a ch re-derive repopulates them.
    account_id   String DEFAULT '',
    asset        String DEFAULT '',
    -- Stroop balance for account (native) + trustline entries, 0 otherwise; lets
    -- top-holder reads aggregate in SQL without decoding XDR.
    balance      Int64 DEFAULT 0,
    -- Position of this change within its ledger's canonical entry-change walk: a
    -- per-LEDGER monotonic counter (change_index restarts per transaction),
    -- assigned in LEDGER-WIDE PHASE order:
    --   phase 1  every tx's fee changes            (tx-set apply order)
    --   phase 2  every tx's apply-phase meta       (tx-changes-before, per-op
    --                                               changes in op_index /
    --                                               change_index order,
    --                                               tx-changes-after)
    --   phase 3  every tx's post-apply fee changes (P23 Soroban refunds)
    -- This mirrors the SDK's canonical ingest.LedgerChangeReader state machine
    -- (feeChangesState -> metaChangesState -> postTxApplyState) and
    -- dispatcher.walkLedgerEntryChanges. A per-transaction order would rank tx1's
    -- apply-phase change BELOW tx2's fee change and let FINAL keep a fee-phase row
    -- as a key's ledger-final state.
    -- Folded into ledger_entries_current's ReplacingMergeTree version so the LAST
    -- change to a key within one ledger wins FINAL dedup deterministically. DEFAULT
    -- 0 is old-binary-safe and what legacy rows carry until re-derived; snapshot/seed
    -- backfill rows stamp 4294967295 (math.MaxUint32, authoritative final state).
    --
    -- POSITIONS ARE SCOPED TO dispatcher.EntryWalkVersion (currently 3). Version 3
    -- renumbered every ledger, so a legacy row can carry a HIGHER version than the
    -- corrected row for the same key, and a lower RMT version never displaces a
    -- higher one. Re-deriving a range therefore requires DROPPING the partition
    -- first, not just re-inserting. See migrations/0120 and
    -- docs/operations/runbooks/entry-walk-renumbering.md.
    intra_ledger_seq UInt32 DEFAULT 0,
    INDEX idx_lec_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    -- key_xdr is not in the sort key; the bloom prunes point lookups. 0.01 matches
    -- r1: a probe reads the whole index, so a tighter FP is slower and no reader
    -- needs it.
    INDEX idx_lec_key_xdr key_xdr TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index, change_index);

-- Current-state projection of ledger_entry_changes: the LATEST entry per
-- (entry_type, key). ReplacingMergeTree keeps the highest-VERSION row; FINAL forces
-- read-time dedup. Backs the account-state + asset-holder reads (ADR-0038 Phase C):
-- a read touches ~1 row per live entry via the account_id / asset skip-indexes
-- instead of a GROUP BY over all historical changes. Kept current by the
-- materialized view below (live-capture and ch-backfill re-derive both flow through).
--
-- Version = (ledger_seq << 32) | intra_ledger_seq, NOT ledger_seq alone. A single
-- ledger can hold several changes to one key (update-then-remove, or
-- remove-then-recreate); with ledger_seq alone they tie and FINAL keeps an
-- ARBITRARY one, which could resurrect a deleted entry or serve a mid-ledger
-- state. Folding the canonical within-ledger walk position (see
-- ledger_entry_changes.intra_ledger_seq) into the low 32 bits makes the version
-- strictly monotonic, so FINAL keeps the LAST change (including a removal).
-- version is MATERIALIZED, so the MV and any reproject INSERT supply only the
-- base columns.
--
-- intra_ledger_seq is 0 on legacy rows until a full re-derive of
-- ledger_entry_changes repopulates it; their same-ledger ties stay unbroken until
-- then. Existing deployments reproject this table via the operator-run
-- deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql (a fresh deploy
-- gets the corrected shape from this file).
CREATE TABLE IF NOT EXISTS stellar.ledger_entries_current
(
    entry_type  LowCardinality(String),
    key_xdr     String,
    account_id  String DEFAULT '',
    asset       String DEFAULT '',
    balance     Int64 DEFAULT 0,
    change_type LowCardinality(String),
    ledger_seq  UInt32,
    close_time  DateTime('UTC'),
    entry_xdr   String,
    intra_ledger_seq UInt32 DEFAULT 0,
    version     UInt64 MATERIALIZED bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq,
    INDEX idx_lecur_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lecur_asset asset TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(version)
ORDER BY (entry_type, key_xdr);

-- Feeds ledger_entries_current from every ledger_entry_changes insert. Selects
-- intra_ledger_seq so the target's MATERIALIZED version folds it in; version
-- itself is computed on insert and is NOT selected here.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ledger_entries_current_mv
TO stellar.ledger_entries_current AS
SELECT entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr, intra_ledger_seq
FROM stellar.ledger_entry_changes;

-- Slim TTL-liveness projection: key_hash -> liveUntilLedgerSeq. Backs
-- internal/storage/clickhouse/ttl_liveness.go's ClassifyTTLLiveness as a
-- primary-key lookup; scanning ledger_entries_current's ttl rows read the wide
-- entry_xdr per row and OOM'd its memory pin. Three tiny columns, one row per TTL
-- change, so the reader is bounded by construction. The binary hard-errors if this
-- table is absent; there is no scan fallback.
--
-- Extraction (ttl_liveness_test.go pins these expressions against the Go layout
-- constants, here AND in deploy/clickhouse/ttl_live_until.sql: keep all three in
-- lockstep): a TTL LedgerKey is 36 bytes (type=00000009 | sha256(LedgerKey)),
-- a TTLEntry is 48 bytes (lastModified(4) | data.type(4) | keyHash(32) |
-- liveUntilLedgerSeq(4) | ext(4)); XDR is big-endian, reinterpretAsUInt32 is
-- little-endian, hence reverse(). version = (ledger_seq << 32) |
-- intra_ledger_seq, same composite as ledger_entries_current.
--
-- Fail-open guard: rows failing the exact 36/48 decoded-length checks are
-- SKIPPED (→ key absent → TTLUnknown → callers KEEP the entry) — a layout
-- change can never fabricate an "archived" verdict. tryBase64Decode, not
-- base64Decode: inside an MV a decode throw would fail the whole source
-- INSERT and block ingest. removals ('' entry_xdr) are likewise skipped; the
-- last parseable live_until (already lapsed for a removed TTL) is retained.
--
-- Existing deployments (r1): table + MV + windowed backfill are operator-run
-- via deploy/clickhouse/ttl_live_until.sql — the MV here only covers ingest
-- from its creation onward, so pre-existing ledger_entry_changes history needs
-- that artifact's Step-2 backfill.
CREATE TABLE IF NOT EXISTS stellar.ttl_live_until
(
    key_hash   FixedString(32),
    live_until UInt32,
    version    UInt64
)
ENGINE = ReplacingMergeTree(version)
ORDER BY key_hash;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ttl_live_until_mv
TO stellar.ttl_live_until AS
SELECT
    toFixedString(substring(tryBase64Decode(key_xdr), 5, 32), 32) AS key_hash,
    reinterpretAsUInt32(reverse(substring(tryBase64Decode(entry_xdr), 41, 4))) AS live_until,
    bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq AS version
FROM stellar.ledger_entry_changes
WHERE entry_type = 'ttl'
  AND length(tryBase64Decode(key_xdr)) = 36
  AND length(tryBase64Decode(entry_xdr)) = 48;

-- Per-token supply events (CAP-67 classic SAC + SEP-41 mint/burn/clawback) with
-- the i128 amount DECODED at ingest (ADR-0034). Total supply for a token is a
-- pure SQL sum over this table:
--   Σ amount WHERE kind='mint' − Σ amount WHERE kind IN ('burn','clawback')
-- No XDR decode at read time and no periodic rollup refresh (the dual-sink keeps it
-- real-time; ch-backfill re-fills holes). ORDER BY contract_id first makes a
-- per-token read a PK-prefix scan; the (ledger,tx,op,event) suffix is the event
-- identity, so re-ingest is idempotent.
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

-- ── contract_events_daily — pre-aggregated per-contract activity ──────────
-- Serves /v1/protocols/{name} detail (event breakdown + activity series) without
-- the ~15s raw scan. The source table is ReplacingMergeTree, so a SummingMergeTree
-- MV would OVERCOUNT on duplicate inserts (live-sink retries, ch-rebuild re-derives
-- re-inserting parts); `events` must dedup on the event's natural key
-- (ledger_seq, tx_hash, op_index, event_index), not sum row counts.
--
-- events uses uniqCombined(17), NOT uniqExact. uniqExact's state is a hash set
-- growing ~16 bytes per distinct event, unbounded for a hot
-- contract+day+event_type+topic group; its background merges exceeded the kernel
-- commit budget (vm.overcommit_memory=2) and starved the live sink.
-- uniqCombined(17) hashes the same natural key into a bounded sketch (~10-96KB
-- per state), so it still collapses duplicate/retried keys to one while merging in
-- bounded memory. The count is an estimate (~0.1-0.5% error at 500K-4M
-- uniques/state); this table is a dashboard pre-aggregation, never the ADR-0033
-- completeness oracle.
--
-- Historical fill (run ONCE after creating, off-peak, windowed by ledger_seq on a
-- large existing lake; see deploy/clickhouse/contract_events_daily_v2.sql for the
-- run-heavy-job-wrapped windowed form):
--   INSERT INTO stellar.contract_events_daily
--   SELECT toDate(close_time) AS day, contract_id, event_type,
--          topic_0_sym, if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
--          if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
--          uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index)
--   FROM stellar.contract_events FINAL
--   GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;
--
-- Changing the table's shape on an EXISTING deployment: an AggregateFunction
-- column's serialized state is tied to its declared function+params, so neither
-- the t0_xdr column (in the ORDER BY) nor the uniqExact -> uniqCombined swap can
-- be a bare ALTER; both need recreate + re-fill:
--   RENAME TABLE stellar.contract_events_daily TO stellar.contract_events_daily_old;
--   -- (also drop/recreate the _mv), run this CREATE, then the fill above,
--   -- then DROP the _old table. Until the new table is populated the fast
--   -- query errors and ProtocolEventBreakdown/ProtocolDailyActivity
--   -- gracefully fall back to the raw scan.
-- The uniqCombined swap ships as a side-by-side v2 build so r1 never runs with the
-- fast path down: see deploy/clickhouse/contract_events_daily_v2.sql for the apply
-- sequence (the CREATE below is what a FRESH deployment gets; IF NOT EXISTS is a
-- no-op against r1's existing v1 table).
CREATE TABLE IF NOT EXISTS stellar.contract_events_daily
(
    day          Date,
    contract_id  String,
    event_type   LowCardinality(String),
    topic_0_sym  LowCardinality(String),
    -- topic[1] raw XDR, captured ONLY when topic[0] isn't a Symbol —
    -- preserves ProtocolEventBreakdown's name-recovery for
    -- soroswap-style [String("SoroswapPair"), Symbol(name)] events.
    t1_xdr       String,
    -- topic[0] raw XDR, captured ONLY when topic[0] isn't a Symbol —
    -- recovers the action name for protocols whose topic[0] IS the event
    -- name but emitted as a non-Symbol scval (Phoenix: [String("swap"),
    -- String("<field>")]). Decoded at read time by effectiveEventName.
    t0_xdr       String,
    events       AggregateFunction(uniqCombined(17), UInt32, String, UInt32, UInt32)
)
ENGINE = AggregatingMergeTree
ORDER BY (contract_id, day, event_type, topic_0_sym, t1_xdr, t0_xdr);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_events_daily_mv
TO stellar.contract_events_daily AS
SELECT
    toDate(close_time) AS day,
    contract_id,
    event_type,
    topic_0_sym,
    if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
    if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
    uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index) AS events
FROM stellar.contract_events
GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;

-- ── tx_hash_index — hash-ordered transaction lookup ────────
-- GET /v1/tx/{hash} resolution table. stellar.transactions is ORDER BY
-- (ledger_seq, tx_index); its tx_hash bloom skip-index prunes but cannot SEEK (a
-- point lookup still scans ~96M residual rows). This table is ORDER BY tx_hash, so
-- hash -> ledger_seq is a primary-key lookup; the reader then re-reads the summary
-- row ledger-scoped.
--
-- ReplacingMergeTree: duplicate inserts (live-sink retries, re-derives, overlapping
-- backfill windows) collapse on merge; tx hashes are unique network-wide, so ORDER
-- BY tx_hash is the row's natural identity. The MV indexes every NEWLY ingested
-- transaction; existing history needs the one-time windowed operator backfill
-- (resumable; see the ClickHouse-log/root-fill caution in
-- docs/operations/perf-todo.md §4):
--
--   stellarindex-ops ch-txindex-backfill -ch-addr 127.0.0.1:9300 \
--     -full -window 5000000 -write
--
-- CORRECTNESS, not just speed: the reader (ExplorerReader.TransactionByHash)
-- treats an index miss as authoritative absence only when the index is non-empty
-- AND stellar.tx_hash_index_coverage holds a marker; a plain miss never falls back
-- to the bloom scan (garbage hashes would trigger a multi-second scan of the full
-- transactions table). Without the marker, or on an index-path error or empty
-- index, the bloom scan answers.
--
-- A freshly (re)created index on a lake with history goes non-empty after the
-- first live transaction while prior history is missing; the marker keeps the
-- reader on the bloom scan until the full backfill has completed:
--
--   stellarindex-ops ch-txindex-backfill -ch-addr 127.0.0.1:9300 \
--     -full -window 5000000 -write
--
-- COVERAGE: the reader cannot prove coverage from the lake data (a row-count
-- comparison over-counts: stellar.transactions is duplicate-bearing while a
-- backfill inserts FINAL-deduped rows). It therefore requires a row in
-- stellar.tx_hash_index_coverage, written by ch-txindex-backfill when a run
-- over the whole history (-full: ledger 2 to the contiguous tip) completes;
-- a partial or explicit -to run writes none. Without it the bloom scan answers.
-- A fresh lake with no history still needs one (trivial) backfill run.
-- Whenever tx_hash_index is dropped and recreated, TRUNCATE
-- stellar.tx_hash_index_coverage too: a stale marker over an emptied index
-- makes every historical hash lookup answer not-found.
CREATE TABLE IF NOT EXISTS stellar.tx_hash_index_coverage
(
    covered_from UInt32,
    covered_to   UInt32,
    completed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (covered_from, covered_to);

CREATE TABLE IF NOT EXISTS stellar.tx_hash_index
(
    tx_hash     String,
    ledger_seq  UInt32,
    tx_index    UInt32,
    ingested_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY tx_hash;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.tx_hash_index_mv
TO stellar.tx_hash_index AS
SELECT tx_hash, ledger_seq, tx_index FROM stellar.transactions;

-- A fee bump's INNER hash resolves to the same row: it is the hash the
-- submitter's SDK returned, so a miss on it would be a wrong 404.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.tx_hash_index_inner_mv
TO stellar.tx_hash_index AS
SELECT inner_tx_hash AS tx_hash, ledger_seq, tx_index FROM stellar.transactions
WHERE inner_tx_hash != '';

-- ── account_movements — ADR-0048 D2 feed-shaped account-activity archive ──
-- Amends ADR-0047 D1 (which planned a Postgres `classic_movements` hypertable):
-- serve by query shape, not by data age. "Enter an address, see everything it has
-- ever done" is `WHERE address = X ORDER BY ledger` over what will become 10-20B
-- immutable rows; that is a ClickHouse-shaped read (ADR-0048 §Context). This table
-- is NOT a raw-lake table (it is decoder-DERIVED, unlike every table above it) but
-- a dedicated SERVING table per ADR-0048 D1, populated by
-- `stellarindex-ops classic-movements-backfill` (internal/sources/classicmovements
-- decodes stellar.operations / operation_results / ledger_entry_changes;
-- internal/storage/clickhouse's FanOutAccountMovement + InsertAccountMovements
-- write here). Never written by the dual-sink / live extractor.
--
-- Feed-shaped: TWO rows per movement (one per participant), with a `direction`
-- discriminator (sent/received/self) rather than from/to columns, so a
-- single-address query needs no OR / UNION. Row cardinality per movement_kind
-- mirrors internal/sources/classicmovements' FromAddress/ToAddress decode (see that
-- package's doc.go and README.md):
--   payment / create_account / clawback / account_merge
--     -> 2 rows (from_address != to_address, both known: one 'sent' row for
--        the source, one 'received' row for the destination)
--   payment, degenerate self-payment (from_address == to_address)
--     -> 1 row, direction='self' (never sent+received for the same address —
--        see FanOutAccountMovement's doc)
--   path_payment
--     -> 2 rows per op, one per asset leg: leg_index 0 'sent' for the source
--        in the send asset, leg_index 1 'received' for the destination in the
--        delivered asset — also for a path payment to self (counterparty=''
--        on both: each asset crosses the path's offers/pools, not the other
--        account; attributes.from/to name both accounts)
--   claimable_balance_create
--     -> 1 row (creator known, claimant unset at creation time — a create can
--        name zero, one, or many eventual claimants; direction='sent',
--        counterparty='')
--   claimable_balance_claim / claimable_balance_clawback
--     -> 1 row (the claimant/issuer performing the action is known; the
--        other side is the escrow, not a G-account; direction='received',
--        counterparty='')
--   liquidity_pool_deposit / liquidity_pool_withdraw
--     -> 2 rows per op (1 row per pool-asset leg x 2 legs; the other side of
--        every leg is always the pool itself, which has no G-account address)
--   CAP-0038 auto-liquidation edge (Phase 4, attributes.revocation=true):
--   liquidity_pool_withdraw + claimable_balance_create
--     -> 2 rows per created ClaimableBalanceEntry: one liquidity_pool_withdraw
--        and one claimable_balance_create for the same balance, each 1 row
--        (trustor known, destination escrow unknown; direction='sent') -- 4
--        for a real liquidation (every classic AMM pool has exactly two
--        assets)
--
-- Engine: ReplacingMergeTree(ingested_at), same idempotent-re-derivation
-- convention as every table above: re-running an already-written
-- classic-movements-backfill window is a safe no-op once merges settle.
--
-- ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction) is ADR-0048
-- D2's exact key: `address` first makes a per-account read a contiguous
-- primary-key range scan; the remainder is the row's natural unique identity within
-- one account's feed. `direction` is LAST deliberately: the sent/received pair of
-- one movement lands at two DIFFERENT `address` values, so it exists only to keep a
-- 'self' row from colliding with the address's own sent/received rows at the same
-- (ledger, tx_hash, op_index, leg_index).
--
-- amount: Int128, matching stellar.supply_flows. Classic amounts are
-- int64-stroop-scale and NOT special-cased (ADR-0047 D1); Int128 keeps every
-- amount column in the lake/serving tier uniformly wide.
--
-- attributes: JSON-as-String (not a native JSON/Map type), mirroring migration
-- 0105's `attributes jsonb` remainder (balance_id, claimants, send_asset/send_amount,
-- dest_asset/dest_amount, pool_id, revocation, ...), read via
-- JSONExtractString/JSONExtract at query time, never a hot-path SQL predicate.
-- FindClaimableBalanceCreates' balance_id lookup is an external-table semijoin run
-- with use_skip_indexes=0, so idx_cb_balance_id below does not back it (no current
-- reader uses it).
CREATE TABLE IF NOT EXISTS stellar.account_movements
(
    address           String,
    ledger            UInt32,
    ledger_close_time DateTime64(0, 'UTC'),
    tx_hash           String,
    op_index          UInt32,
    leg_index         UInt32,
    direction         LowCardinality(String),
    movement_kind     LowCardinality(String),
    provenance        LowCardinality(String),
    asset             String,
    counterparty      String DEFAULT '',
    amount            Int128,
    attributes        String DEFAULT '{}',
    ingested_at       DateTime DEFAULT now(),
    -- Prunes only when the WHERE predicate is textually IDENTICAL to this
    -- expression; the batched balance_id lookup runs with use_skip_indexes=0.
    INDEX idx_cb_balance_id JSONExtractString(attributes, 'balance_id') TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction);

-- ── movements_by_asset — asset-keyed copy of account_movements ──
-- "Everything that ever moved asset X" is a scan over account_movements, whose
-- ORDER BY leads with address. Same rows, same columns, asset-first key; the
-- trailing address makes the sent/received pair of one movement two distinct
-- keys so ReplacingMergeTree never collapses them. Live rows arrive through
-- the MV; history is caught up by deploy/clickhouse/movements_by_asset.sql
-- (partition-at-a-time INSERT..SELECT FROM account_movements FINAL, plus the
-- DROP PARTITION note for asset-relabelling re-derives). Keep that file's DDL identical.
CREATE TABLE IF NOT EXISTS stellar.movements_by_asset
(
    address           String,
    ledger            UInt32,
    ledger_close_time DateTime64(0, 'UTC'),
    tx_hash           String,
    op_index          UInt32,
    leg_index         UInt32,
    direction         LowCardinality(String),
    movement_kind     LowCardinality(String),
    provenance        LowCardinality(String),
    asset             String,
    counterparty      String DEFAULT '',
    amount            Int128,
    attributes        String DEFAULT '{}',
    ingested_at       DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (asset, ledger, tx_hash, op_index, leg_index, direction, address);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.movements_by_asset_mv
TO stellar.movements_by_asset AS
SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
       movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
FROM stellar.account_movements;

-- ── ops_by_source: slim sourced-history projection ─────────────
-- (source_account -> ledger/tx/op keys) from BOTH stellar.operations (op-effective
-- source, real op_index) and stellar.transactions (tx source, sentinel op_index
-- 4294967295). The account-history readers' sourced arms read this via PK prefix
-- instead of the source_account bloom probe over the full lake tables. The reader
-- REFUSES account-history reads without this table (no scan fallback). Keep in
-- lockstep with deploy/clickhouse/ops_by_source.sql (which also carries the Step-2
-- windowed backfills a fresh MV-only deployment still needs for history).
CREATE TABLE IF NOT EXISTS stellar.ops_by_source
(
    source_account String,
    ledger_seq     UInt32,
    tx_index       UInt32,
    op_index       UInt32
)
ENGINE = ReplacingMergeTree
ORDER BY (source_account, ledger_seq, tx_index, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_ops_mv
TO stellar.ops_by_source AS
SELECT source_account, ledger_seq, tx_index, op_index
FROM stellar.operations
WHERE source_account != '';

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_tx_mv
TO stellar.ops_by_source AS
SELECT source_account, ledger_seq, tx_index, 4294967295 AS op_index
FROM stellar.transactions
WHERE source_account != '';

-- ── account_op_types — see deploy/clickhouse/account_op_types.sql ──
-- (account -> op key, op_type) for every op an account's history shows: ops it
-- sourced (the MV below) and ops it took part in as a non-source participant whose
-- tx is visible to it (successful, or sourced by it), written by the CH sink beside
-- operation_participants. AccountOperationTypeCounts reads it by PK prefix instead
-- of joining account keys into stellar.operations, and only while
-- account_op_types_coverage holds a marker (TRUNCATE the marker whenever the table
-- is rebuilt). Keep in lockstep with deploy/clickhouse/account_op_types.sql.
CREATE TABLE IF NOT EXISTS stellar.account_op_types
(
    account    String,
    ledger_seq UInt32,
    tx_index   UInt32,
    op_index   UInt32,
    op_type    LowCardinality(String)
)
ENGINE = ReplacingMergeTree
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (account, ledger_seq, tx_index, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_op_types_source_mv
TO stellar.account_op_types AS
SELECT source_account AS account, ledger_seq, tx_index, op_index, op_type
FROM stellar.operations
WHERE source_account != '';

CREATE TABLE IF NOT EXISTS stellar.account_op_types_coverage
(
    covered_from UInt32,
    covered_to   UInt32,
    completed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (covered_from, covered_to);

-- Step 6's rollup: per lake partition, count() FINAL of account_op_types by
-- (account, op_type), plus one sentinel row (account '', op_type '') whose
-- `through` is the ledger the build stopped before and whose `n` is the source
-- partition's active row count at build start.
CREATE TABLE IF NOT EXISTS stellar.account_op_type_counts
(
    part    UInt32,
    account String,
    op_type LowCardinality(String),
    n       UInt64,
    through UInt32
)
ENGINE = MergeTree
PARTITION BY part
ORDER BY (account, op_type)
SETTINGS index_granularity = 1024;

-- The movements listing's partition directory, built by the same command:
-- count() FINAL of account_movements by (address, movement_kind), same
-- sentinel scheme (account '', movement_kind '').
CREATE TABLE IF NOT EXISTS stellar.account_movement_counts
(
    part          UInt32,
    account       String,
    movement_kind LowCardinality(String),
    n             UInt64,
    through       UInt32
)
ENGINE = MergeTree
PARTITION BY part
ORDER BY (account, movement_kind)
SETTINGS index_granularity = 1024;

-- ── contract_event_shapes — see deploy/clickhouse/contract_event_shapes.sql ──
-- One row per distinct recognition shape (contract_id, topic_0_sym and, for a
-- non-Symbol topic[0], topics_xdr[1], topics_xdr[2], arity) per lake partition,
-- with row count and first/last ledger. DistinctTopicShapes reads whole partitions
-- from here instead of rescanning contract_events, only while
-- contract_event_shapes_coverage holds a marker (TRUNCATE the marker whenever the
-- table is rebuilt). Keep in lockstep with deploy/clickhouse/contract_event_shapes.sql.
CREATE TABLE IF NOT EXISTS stellar.contract_event_shapes
(
    part        UInt32,
    contract_id String,
    topic_0_sym String,
    t0          String,
    t1          String,
    tn          UInt64,
    cnt         SimpleAggregateFunction(sum, UInt64),
    lo          SimpleAggregateFunction(min, UInt32),
    hi          SimpleAggregateFunction(max, UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY part
ORDER BY (contract_id, topic_0_sym, t0, t1, tn);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_event_shapes_mv
TO stellar.contract_event_shapes AS
SELECT
    intDiv(ledger_seq, 1000000) AS part,
    contract_id,
    topic_0_sym,
    if(topic_0_sym = '', topics_xdr[1], '') AS t0,
    if(topic_0_sym = '', topics_xdr[2], '') AS t1,
    if(topic_0_sym = '', length(topics_xdr), 0) AS tn,
    count() AS cnt,
    min(ledger_seq) AS lo,
    max(ledger_seq) AS hi
FROM stellar.contract_events
GROUP BY part, contract_id, topic_0_sym, t0, t1, tn;

CREATE TABLE IF NOT EXISTS stellar.contract_event_shapes_coverage
(
    covered_from UInt32,
    covered_to   UInt32,
    completed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (covered_from, covered_to);

-- ── account_activity — see deploy/clickhouse/account_activity.sql ──
-- Per-account activity watermark: (account -> the max ledger the account was EVER
-- named in, across EVERY role the account-history readers serve: op source, tx
-- source/fee-payer, non-source participant). ~1 row per account. AccountOperations
-- reads max(last_ledger) as an EXACT UPPER BOUND (`ledger_seq <= ?`) on its per-arm
-- reverse primary-key resolves, so a long-idle account's page stops at its real
-- last activity instead of walking granules back from the tip.
--
-- CORRECTNESS INVARIANT (data-hiding — read before touching the MV set): the
-- watermark must be >= the ledger_seq of every row the account-history
-- queries can return for the account. The three MVs below fire on inserts to
-- the SAME tables that feed those queries' key sources (stellar.operations →
-- ops_by_source's ops arm, stellar.operation_participants, and
-- stellar.transactions → ops_by_source's tx-sentinel arm), so any row that
-- can surface — including a Soroban/SAC movement that names a long-idle
-- classic account only as a participant — also raised the watermark to at
-- least its own ledger. A too-HIGH watermark only costs scan range; a
-- too-LOW one HIDES DATA. Never narrow the MV set below the readers'
-- account-role set. Readers take max() across un-merged RMT rows and fall
-- back to the UNBOUNDED scan when an account has no row.
--
-- A fresh host gets these MVs before any ingest, so they see every row and
-- the invariant holds from the start. A host whose lake PREDATES the MVs
-- must run account_activity.sql's Step-2 backfill before deploying a reader:
-- until then a re-ingest of old ledgers can create an account's ONLY row
-- below its true last activity — a too-LOW bound (see the HAZARD there).
CREATE TABLE IF NOT EXISTS stellar.account_activity
(
    account_id  String,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = ReplacingMergeTree(last_ledger)
ORDER BY account_id;

-- Per-block GROUP BY: each MV collapses an insert block to one row per
-- account (a busy block names the same account many times); RMT merges
-- keep the max-last_ledger row across blocks, and readers max() over
-- whatever is not yet merged.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_ops_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operations
WHERE source_account != ''
GROUP BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_tx_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.transactions
WHERE source_account != ''
GROUP BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_participants_mv
TO stellar.account_activity AS
SELECT account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operation_participants
GROUP BY account_id;

-- ── contract_active_ledgers — see deploy/clickhouse/contract_active_ledgers.sql ──
CREATE TABLE IF NOT EXISTS stellar.contract_active_ledgers
(
    contract_id String,
    ledger_seq  UInt32,
    close_time  DateTime('UTC'),
    ingested_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (contract_id, ledger_seq);

-- SELECT DISTINCT collapses within each insert block (a busy AMM emits many
-- events per contract-ledger); RMT merges collapse the rest across blocks.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_active_ledgers_mv
TO stellar.contract_active_ledgers AS
SELECT DISTINCT contract_id, ledger_seq, close_time
FROM stellar.contract_events;

-- ── cap67_movements_watermark — see deploy/clickhouse/cap67_movements.sql ──
CREATE TABLE IF NOT EXISTS stellar.cap67_movements_watermark
(
    name        String,
    thru_ledger UInt32,
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name;

-- ── asset_holders_rollup — see deploy/clickhouse/asset_holders_rollup.sql ──
CREATE TABLE IF NOT EXISTS stellar.asset_holders_rollup
(
    asset       String,
    rank        UInt32,
    account_id  String,
    balance     Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (asset, rank);

CREATE TABLE IF NOT EXISTS stellar.asset_holders_rollup_staging
AS stellar.asset_holders_rollup;

CREATE TABLE IF NOT EXISTS stellar.asset_holders_counts
(
    asset       String,
    holders     Int64,
    circulating Int128 DEFAULT 0,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY asset;

CREATE TABLE IF NOT EXISTS stellar.asset_holders_counts_staging
AS stellar.asset_holders_counts;

-- Per-asset daily concentration snapshot, written by the same cycle after
-- its swap; the day is recomputed whole and swapped in by REPLACE PARTITION.
-- Balance sums are Int128: a classic asset's per-trustline Int64 balances
-- can sum past 2^63. gini is over positive-balance holders, NULL when none.
CREATE TABLE IF NOT EXISTS stellar.asset_stats_daily
(
    day            Date,
    asset          String,
    holders        Int64,
    trustlines     Int64,
    balance_total  Int128,
    top10_balance  Int128,
    top100_balance Int128,
    gini           Nullable(Float64),
    computed_at    DateTime DEFAULT now()
)
ENGINE = MergeTree
PARTITION BY day
ORDER BY (asset, day);

CREATE TABLE IF NOT EXISTS stellar.asset_stats_daily_staging
AS stellar.asset_stats_daily;

-- ── accounts_stats rollup — see deploy/clickhouse/accounts_stats_rollup.sql ──
CREATE TABLE IF NOT EXISTS stellar.accounts_stats
(
    metric      String,
    value       Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY metric;

CREATE TABLE IF NOT EXISTS stellar.accounts_stats_staging
AS stellar.accounts_stats;

-- Wealth histogram: bucket = clamp(floor(log10(balance_xlm)), -1..10);
-- -1 is "< 1 XLM", 10 is ">= 10B XLM".
CREATE TABLE IF NOT EXISTS stellar.accounts_wealth_histogram
(
    bucket      Int8,
    accounts    UInt64,
    xlm_stroops Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY bucket;

CREATE TABLE IF NOT EXISTS stellar.accounts_wealth_histogram_staging
AS stellar.accounts_wealth_histogram;

-- Trustlines-per-account histogram (accounts with >= 1 trustline; the
-- zero bucket is derived API-side from total_accounts).
CREATE TABLE IF NOT EXISTS stellar.accounts_trustline_histogram
(
    bucket      String,
    accounts    UInt64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY bucket;

CREATE TABLE IF NOT EXISTS stellar.accounts_trustline_histogram_staging
AS stellar.accounts_trustline_histogram;

-- ── account_creators — see deploy/clickhouse/account_creators_rollup.sql ──
-- Funder → created-account league table, aggregated from the
-- create_account arm of stellar.account_movements. funded_stroops and
-- live_stroops are Int128 to match account_movements.amount; both are
-- served as decimal strings (ADR-0003). account_creators_ops is a
-- per-cycle working table, not a served one: the archive pass is walked
-- one lake partition at a time and lands there, so the board's join
-- against the live account entries is paid once per cycle.
CREATE TABLE IF NOT EXISTS stellar.account_creators_ops
(
    creator   String,
    created   String,
    amount    Int128,
    ledger    UInt32,
    closed_at DateTime('UTC')
)
ENGINE = MergeTree
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (creator, ledger, created);

CREATE TABLE IF NOT EXISTS stellar.account_creators_rollup
(
    rank             UInt32,
    creator          String,
    accounts_created UInt64,
    funded_stroops   Int128,
    live_accounts    UInt64,
    live_stroops     Int128,
    first_ledger     UInt32,
    last_ledger      UInt32,
    first_created_at DateTime('UTC'),
    last_created_at  DateTime('UTC'),
    computed_at      DateTime DEFAULT now(),
    -- The sort key serves the top-N page; this serves ?account=.
    INDEX idx_creators_rollup_creator creator TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
ORDER BY rank;

CREATE TABLE IF NOT EXISTS stellar.account_creators_rollup_staging
AS stellar.account_creators_rollup;

-- Metric-keyed (adding a figure is an INSERT, not a migration). Carries
-- the exact totals AND the data-derived coverage span the cycle read:
-- creators_total, creations_total, live_accounts_total, from_ledger,
-- thru_ledger, from_time, thru_time.
CREATE TABLE IF NOT EXISTS stellar.account_creators_stats
(
    metric      String,
    value       Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY metric;

CREATE TABLE IF NOT EXISTS stellar.account_creators_stats_staging
AS stellar.account_creators_stats;

-- Creation GRAPH edges — one row per DISTINCT (creator, created)
-- pair, held in both sort orders so each direction of "who created whom"
-- is a primary-key range read. The collapse to distinct pairs is what
-- bounds the inbound direction: an address can be created, merged and
-- created again, and the widest such address measured on r1 carries
-- 29,634 creation events but only ONE creator. Full rationale and the
-- measurements in deploy/clickhouse/account_creators_rollup.sql.
CREATE TABLE IF NOT EXISTS stellar.account_creator_edges
(
    creator        String,
    created        String,
    creations      UInt64,
    funded_stroops Int128,
    first_ledger   UInt32,
    last_ledger    UInt32,
    first_at       DateTime('UTC'),
    last_at        DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (creator, created);

CREATE TABLE IF NOT EXISTS stellar.account_creator_edges_staging
AS stellar.account_creator_edges;

CREATE TABLE IF NOT EXISTS stellar.account_creator_edges_by_created
(
    created        String,
    creator        String,
    creations      UInt64,
    funded_stroops Int128,
    first_ledger   UInt32,
    last_ledger    UInt32,
    first_at       DateTime('UTC'),
    last_at        DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (created, creator);

CREATE TABLE IF NOT EXISTS stellar.account_creator_edges_by_created_staging
AS stellar.account_creator_edges_by_created;


-- ── account_sponsors — see deploy/clickhouse/account_sponsors_rollup.sql ──
-- Sponsor -> sponsored-account league table, aggregated from the
-- Begin/End/Revoke sponsorship operations in stellar.operations. The
-- sponsored identity comes from the End operation's source_account, so
-- no body_xdr is read. account_sponsors_ops is a per-cycle working
-- table, not a served one.
CREATE TABLE IF NOT EXISTS stellar.account_sponsors_ops
(
    lseq     UInt32,
    tidx     UInt32,
    oidx     UInt32,
    otype    LowCardinality(String),
    src      String,
    ctime    DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (lseq, tidx, oidx);

CREATE TABLE IF NOT EXISTS stellar.account_sponsors_rollup
(
    rank                 UInt32,
    sponsor              String,
    -- Immutable history: sponsorship arrangements this account has
    -- STARTED, and how many distinct accounts it has ever sponsored.
    -- These only grow.
    sponsorships_started UInt64,
    distinct_sponsored   UInt64,
    -- Revocations this account ISSUED (it was the source of a
    -- RevokeSponsorship). Also history, and the reason sponsorships
    -- started is not a count of sponsorships still in force.
    revocations_issued   UInt64,
    first_ledger         UInt32,
    last_ledger          UInt32,
    first_seen_at        DateTime('UTC'),
    last_seen_at         DateTime('UTC'),
    computed_at          DateTime DEFAULT now(),
    -- The sort key serves the top-N page; this serves ?account=.
    INDEX idx_sponsors_rollup_sponsor sponsor TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
ORDER BY rank;

CREATE TABLE IF NOT EXISTS stellar.account_sponsors_rollup_staging
AS stellar.account_sponsors_rollup;

CREATE TABLE IF NOT EXISTS stellar.account_sponsors_stats
(
    metric      String,
    value       Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY metric;

CREATE TABLE IF NOT EXISTS stellar.account_sponsors_stats_staging
AS stellar.account_sponsors_stats;

-- Sponsorship GRAPH edges — one row per DISTINCT
-- (sponsor, sponsored) pair, held in both sort orders so each direction
-- is a primary-key range read. HISTORY, like the board: an edge means an
-- arrangement was STARTED, never that one is in force, and no per-edge
-- revoked flag exists because RevokeSponsorship names its target inside
-- body_xdr, which this cycle does not decode. Full rationale and the
-- measurements in deploy/clickhouse/account_sponsors_rollup.sql.
CREATE TABLE IF NOT EXISTS stellar.account_sponsor_edges
(
    sponsor              String,
    sponsored            String,
    sponsorships_started UInt64,
    first_ledger         UInt32,
    last_ledger          UInt32,
    first_at             DateTime('UTC'),
    last_at              DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (sponsor, sponsored);

CREATE TABLE IF NOT EXISTS stellar.account_sponsor_edges_staging
AS stellar.account_sponsor_edges;

CREATE TABLE IF NOT EXISTS stellar.account_sponsor_edges_by_sponsored
(
    sponsored            String,
    sponsor              String,
    sponsorships_started UInt64,
    first_ledger         UInt32,
    last_ledger          UInt32,
    first_at             DateTime('UTC'),
    last_at              DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (sponsored, sponsor);

CREATE TABLE IF NOT EXISTS stellar.account_sponsor_edges_by_sponsored_staging
AS stellar.account_sponsor_edges_by_sponsored;

-- account_cohort_* — what the accounts this address CREATED or SPONSORED went on to
-- hold and do: current holdings, monthly flows in and out, the contracts it moved
-- value through, how recently it was active, and its open DeFi positions. The
-- sponsor and creator boards rank by how many accounts an address stood behind;
-- these answer what value that cohort brought.
--
-- One cycle (stellarindex-ops ch-cohort-rollup) rebuilds every table
-- into its _staging twin and swaps atomically, so a reader never sees a
-- half-built cohort. Membership is the edge tables above: an account
-- belongs to the cohort of every root it has an edge from, so the same
-- account can count under its creator AND each of its sponsors — the
-- two relations are never summed.
--
-- COVERED ROOTS. Every sponsor, and every creator with at least
-- account_cohort_min_created accounts to its name (the rollup constant in
-- internal/storage/clickhouse/account_cohort_rollup.go). Below that a
-- cohort is a handful of accounts and the per-account explorer pages are
-- the better read; the API says so rather than serving an empty cohort
-- as "holds nothing".
CREATE TABLE IF NOT EXISTS stellar.account_cohort_roots
(
    rel             LowCardinality(String),   -- 'created' | 'sponsored'
    root            String,
    cohort_accounts UInt64,                   -- distinct members
    live_accounts   UInt64,                   -- members with a live account entry
    computed_at     DateTime('UTC'),
    tip_ledger      UInt32                    -- lake tip the cycle read to
)
ENGINE = MergeTree
ORDER BY (rel, root);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_roots_staging
AS stellar.account_cohort_roots;

-- Working table: the cohort membership the cycle aggregates against,
-- keyed by member so every source table joins on its account column.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_members
(
    rel    LowCardinality(String),
    root   String,
    member String
)
ENGINE = MergeTree
ORDER BY (member, rel, root);

-- Current holdings of the cohort, per asset. `asset` is the canonical id
-- ledger_entries_current carries: 'native', 'CODE-ISSUER', or
-- 'pool:<hex>' for a classic liquidity-pool share (a DeFi position on
-- the classic side). Balances are stroops.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_holdings
(
    rel     LowCardinality(String),
    root    String,
    asset   String,
    holders UInt64,
    balance Int128
)
ENGINE = MergeTree
ORDER BY (rel, root, asset);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_holdings_staging
AS stellar.account_cohort_holdings;

-- Working table: one row per (cohort, month, asset, contract) per ledger
-- window of the movements walk. A month spans windows, so the walk
-- writes partial rows with a mergeable distinct-account state and the
-- cycle folds them into flows and contracts afterwards. `contract` is
-- the C… counterparty of a movement, '' for everything else.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_parts_staging
(
    rel       LowCardinality(String),
    root      String,
    month     Date,
    asset     String,
    contract  String,
    inflow    Int128,
    outflow   Int128,
    movements UInt64,
    actives   AggregateFunction(uniqCombined, String),
    first_at  DateTime('UTC'),
    last_at   DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (rel, root, month, asset, contract);

-- Monthly value moved into and out of the cohort, per asset, in the
-- asset's smallest unit (stroops for classic assets; a Soroban token's
-- own scale when `asset` is a C… id). `movements` counts legs,
-- `active_accounts` distinct members that moved something that month
-- (uniqCombined — a close estimate, not an exact count).
CREATE TABLE IF NOT EXISTS stellar.account_cohort_flows
(
    rel             LowCardinality(String),
    root            String,
    month           Date,
    asset           String,
    inflow          Int128,
    outflow         Int128,
    movements       UInt64,
    active_accounts UInt64
)
ENGINE = MergeTree
ORDER BY (rel, root, month, asset);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_flows_staging
AS stellar.account_cohort_flows;

-- Contracts the cohort moved value through: every C… counterparty of a
-- cohort movement, with how much of the cohort touched it and when.
-- This is the value-moving subset of "interacted with" — a call that
-- moved no balance leaves no movement and is not counted here.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_contracts
(
    rel             LowCardinality(String),
    root            String,
    contract_id     String,
    movements       UInt64,
    active_accounts UInt64,
    first_at        DateTime('UTC'),
    last_at         DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (rel, root, contract_id);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_contracts_staging
AS stellar.account_cohort_contracts;

-- How recently the cohort was active, from account_activity's last-seen
-- watermark per account: members seen in the last 30 / 90 / 365 days
-- as of the cycle.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_activity
(
    rel         LowCardinality(String),
    root        String,
    active_30d  UInt64,
    active_90d  UInt64,
    active_365d UInt64
)
ENGINE = MergeTree
ORDER BY (rel, root);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_activity_staging
AS stellar.account_cohort_activity;

-- A snapshot of every open Soroban DeFi position the served tier's
-- per-protocol folds know (blend, blend backstop, phoenix stake,
-- defindex vaults, sorocredit, aquarius gauges — the same six behind
-- /v1/accounts/{g}/positions), copied from Postgres each cycle so the
-- cohort join happens here, keyed by the position's owner. `amount` is
-- the fold's own decimal string in the fold's own unit; `asset` is ''
-- when the position is denominated in venue shares.
CREATE TABLE IF NOT EXISTS stellar.defi_position_holders
(
    protocol      LowCardinality(String),
    position_kind LowCardinality(String),
    venue         String,
    asset         String,
    user          String,
    amount        String,
    last_ledger   UInt32,
    snapshot_at   DateTime('UTC')
)
ENGINE = MergeTree
ORDER BY (user, protocol, venue, asset, position_kind);

CREATE TABLE IF NOT EXISTS stellar.defi_position_holders_staging
AS stellar.defi_position_holders;

-- One USD price per (canonical asset, calendar month): the served tier's
-- monthly VWAP over its own USD-quoted markets (prices_1mo, every alias
-- spelling of the base folded together — timescale.Store.MonthlyUSDVWAPs),
-- copied from Postgres each cohort cycle. The flows read LEFT JOINs it on
-- (asset, month) to value a month's movement at THAT month's price
-- beside the live-price figure; a month with no USD-quoted market has no
-- row and is served unpriced, never at zero. `vwap_usd` is the exact
-- decimal string; `volume_usd` the month's USD volume behind it. XLM's
-- own row is here under 'native'.
CREATE TABLE IF NOT EXISTS stellar.asset_month_usd_prices
(
    asset      String,
    month      Date,
    vwap_usd   String,
    volume_usd Float64
)
ENGINE = MergeTree
ORDER BY (asset, month);

CREATE TABLE IF NOT EXISTS stellar.asset_month_usd_prices_staging
AS stellar.asset_month_usd_prices;

-- Open DeFi positions held by the cohort, per protocol / venue / asset.
-- `amount` is the exact Int256 sum of the folds' integer amounts, each in
-- the fold's own unit (ADR-0003: never a float).
CREATE TABLE IF NOT EXISTS stellar.account_cohort_positions
(
    rel           LowCardinality(String),
    root          String,
    protocol      LowCardinality(String),
    position_kind LowCardinality(String),
    venue         String,
    asset         String,
    holders       UInt64,
    amount        Int256
)
ENGINE = MergeTree
ORDER BY (rel, root, protocol, venue, asset, position_kind);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_positions_staging
AS stellar.account_cohort_positions;

-- contract_instance_changes — per-contract instance-executable timeline index for
-- the explorer's code-history + wasm reads.
--
-- WHY: stellar.ledger_entry_changes is ORDER BY (ledger_seq, ...), so the
-- "instance entry for contract X" predicate (key_xdr IN (...)) is scan-shaped over
-- the whole history: 8s+ cold and 503 under the explorer budget for any
-- non-prewarmed contract. ContractCodeHistory and the ContractWasm hash resolution
-- both pay it.
--
-- THE INDEX: one narrow row per captured instance-entry WRITE, keyed (contract,
-- ledger, tx_hash, change_index), carrying just the decoded verdict: SAC or wasm +
-- which hash. The wire layout makes the MV extraction a pair of fixed-offset
-- substrings (byte-verified against go-stellar-sdk marshalling, all three shapes):
--
--   key_xdr   (48 bytes): [1-4]=LedgerKey type contract_data(6)
--                         [5-8]=SCAddress type contract(1)
--                         [9-40]=contract id  [41-44]=SCVal type
--                         ScvLedgerKeyContractInstance(0x14)
--                         [45-48]=durability
--   entry_xdr (var):      [57-60]=val type ScvContractInstance(0x13)
--                         [61-64]=executable type (0=wasm 1=SAC)
--                         [65-96]=wasm hash (wasm only)
--
-- Instance-STORAGE writes rewrite the same key, so a busy contract contributes many
-- rows, but each is ~90 bytes vs the multi-KB entry_xdr and readers collapse to
-- distinct executables.
--
-- THE KEY: change_index restarts at 0 on every TRANSACTION, so two transactions in
-- one ledger writing the same contract's instance share it and would merge into one
-- row. (tx_hash, change_index) is the change's identity: unique within a ledger,
-- stable across re-ingest and dispatcher.EntryWalkVersion renumbering, and
-- populated on legacy rows whose intra_ledger_seq is still 0. intra_ledger_seq is
-- carried but NOT keyed: it is the READ order (ledger_seq, intra_ledger_seq,
-- change_index), which puts the ledger-final write last; keying on it would stop a
-- renumbering re-derive from replacing the rows it supersedes. Legacy rows
-- (intra_ledger_seq 0) order across transactions only by change_index until
-- ledger_entry_changes is re-derived; seed rows (4294967295) are ledger-final by
-- construction.
--
-- REPLAY-SAFE: no counts; identical (contract, ledger, tx_hash, change_index) keys
-- re-inserted by re-derives/overlapping backfills collapse in the RMT.
--
-- EXISTING DEPLOYMENTS: an existing host created this table under the old
-- (contract, ledger, change_index) key, and CREATE IF NOT EXISTS is a no-op
-- against it. ORDER BY cannot change in place: run
-- deploy/clickhouse/contract_instance_changes_tx_key.sql (side table, genesis
-- backfill, rename cut-over).
--
-- OPERATOR CONTRACT (same as contract_active_ledgers): presence +
-- non-empty = readers TRUST it, including per-contract emptiness
-- ("instance never captured"). Apply this DDL, then IMMEDIATELY run the
-- windowed historical backfill to genesis — serialized with other heavy
-- jobs:
--
--   /usr/local/sbin/run-heavy-job.sh instance-changes-backfill \
--     /usr/local/bin/stellarindex-ops ch-instance-backfill \
--     -ch-addr 127.0.0.1:9300 -from 2 -window 2000000 -write
--
-- Do NOT leave the table applied-but-unbackfilled on a lake with
-- history: cold contracts would resolve "no wasm" / truncated upgrade
-- timelines stamped as complete.

CREATE TABLE IF NOT EXISTS stellar.contract_instance_changes
(
    contract_hash FixedString(64),  -- lower-hex 32-byte contract id
    ledger_seq    UInt32,
    tx_hash       String DEFAULT '', -- '' on snapshot/seed rows
    change_index  UInt32,           -- per-TRANSACTION position
    intra_ledger_seq UInt32 DEFAULT 0, -- per-LEDGER walk position
    close_time    DateTime('UTC'),
    is_sac        UInt8,            -- executable type: 0 = wasm, 1 = SAC
    wasm_hash     String,           -- lower-hex, '' when is_sac = 1
    ingested_at   DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (contract_hash, ledger_seq, tx_hash, change_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_instance_changes_mv
TO stellar.contract_instance_changes AS
SELECT
    lower(hex(substring(tryBase64Decode(key_xdr), 9, 32)))  AS contract_hash,
    ledger_seq,
    tx_hash,
    change_index,
    intra_ledger_seq,
    close_time,
    toUInt8(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000001')) AS is_sac,
    if(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000000'),
       lower(hex(substring(tryBase64Decode(entry_xdr), 65, 32))), '')         AS wasm_hash
FROM stellar.ledger_entry_changes
WHERE entry_type = 'contract_data'
  AND length(key_xdr) = 64
  AND substring(tryBase64Decode(key_xdr), 1, 8) = unhex('0000000600000001')
  AND substring(tryBase64Decode(key_xdr), 41, 4) = unhex('00000014')
  AND entry_xdr != ''
  AND substring(tryBase64Decode(entry_xdr), 57, 4) = unhex('00000013');
-- contracts_census_daily — day-keyed per-contract event counts behind the
-- /v1/contracts directory.
--
-- WHY: the directory's census query (uniqExact over the PK of contract_events,
-- GROUP BY contract_id over multi-day windows) measured 40s / 790M rows per run.
-- Ranking from contract_events_daily was WORSE (24.5s, merging ~15M uniqCombined
-- HLL states), and contract_active_ledgers is contract-first so a window scan
-- doesn't prune. The durable shape is plain per-day counts computed ONCE per day.
--
-- DUP-SAFETY: this table has NO MV (a Summing-MV would double-count on duplicate
-- inserts). A periodic job (stellarindex-ops ch-census-rollup) recomputes whole
-- days and swaps them in atomically with ALTER TABLE ... REPLACE PARTITION, so
-- recomputing a day is idempotent and a partial write never mixes with served
-- rows. Completed days are written once; the current (partial) day is re-replaced
-- every cycle, so the serving tail is at most one cadence (30 min) stale.
--
-- OPERATOR CONTRACT (same as the sibling indexes): presence +
-- non-empty = the reader TRUSTS it for any window whose floor is
-- covered. Apply the DDL, then run the historical fill promptly:
--
--   /usr/local/sbin/run-heavy-job.sh census-rollup-backfill \
--     /usr/local/bin/stellarindex-ops ch-census-rollup \
--     -ch-addr 127.0.0.1:9300 -backfill
--
-- and install the 30-min timer for the incremental cycle.

CREATE TABLE IF NOT EXISTS stellar.contracts_census_daily
(
    day         Date,
    contract_id String,
    -- uniqExact over (ledger_seq, tx_hash, op_index, event_index): exact, since
    -- count() over-counts lake duplicate rows.
    events      UInt64,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = MergeTree
-- Day, not intDiv(ledger, 1000000) like the lake tables: the day IS the unit
-- ch-census-rollup recomputes and swaps with REPLACE PARTITION '<day>'.
PARTITION BY day
ORDER BY (day, contract_id);

-- Sponsorship lifecycle, one row per transition of one arrangement: the entry
-- (or signer) a sponsor pays reserves for started, was revoked (entry lives on)
-- or ended (entry gone). Written by ch-entry-history under its own watermark,
-- never past entry_index. `sponsored` is empty for a claimable balance; the
-- bloom filter serves the sponsored side, the key the sponsor side.
CREATE TABLE IF NOT EXISTS stellar.sponsorship_events
(
    sponsor          String,
    sponsored        String DEFAULT '',
    kind             LowCardinality(String),
    key_id           String DEFAULT '',
    ledger           UInt32,
    close_time       DateTime('UTC'),
    tx_hash          String,
    op_index         Int32,
    change_index     UInt32,
    intra_ledger_seq UInt32,
    reserves         UInt8,
    event            LowCardinality(String),
    ingested_at      DateTime DEFAULT now(),
    INDEX sponsored_bf sponsored TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (sponsor, sponsored, kind, key_id, ledger, tx_hash, op_index, change_index);

-- One account-side's sponsorship totals per 10,000-ledger bucket, recomputed from
-- sponsorship_events by ch-entry-history: in_force is the bucket's net change, so
-- completed buckets sum to the totals and the route reads only its tip bucket live.
CREATE TABLE IF NOT EXISTS stellar.sponsorship_totals_buckets
(
    account           String,
    side              LowCardinality(String),
    bucket            UInt32,
    in_force          Int64,
    in_force_reserves Int64,
    started           UInt64,
    revoked           UInt64,
    ended             UInt64,
    refreshed_at      DateTime64(6) DEFAULT now64(6)
)
ENGINE = ReplacingMergeTree(refreshed_at)
PARTITION BY intDiv(bucket, 100)
ORDER BY (account, side, bucket);

-- ── entry_history_watermark — see deploy/clickhouse/entry_history.sql ──
CREATE TABLE IF NOT EXISTS stellar.entry_history_watermark
(
    name        String,
    thru_ledger UInt32,
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name;

-- ── account_entry_ledgers / asset_entry_ledgers — the entry index: which ledgers an account
-- (by role) or an asset has classic-entry activity in; no payload. Built by
-- `stellarindex-ops ch-entry-index` (deploy/clickhouse/entry_index.sql carries the runbook).

CREATE TABLE IF NOT EXISTS stellar.account_entry_ledgers
(
    account FixedString(32),
    role    Enum8('owner' = 1, 'claimant' = 2, 'sponsor' = 3, 'signer' = 4),
    ledger  UInt32
)
ENGINE = ReplacingMergeTree
PARTITION BY intDiv(ledger, 10000000)
ORDER BY (account, role, ledger);

-- account_claimables: one row per (claimant, claimable balance) from the same decode pass, carrying the newest entry
-- version seen, so the claimable-balances route reads a key range instead of scanning every change in the claimant
-- ledgers. claimable_removals: the ledger each claimable balance was claimed or clawed back; removal rows name no
-- claimant, so liveness is a key lookup here rather than in the 5B-row ledger_entries_current.
CREATE TABLE IF NOT EXISTS stellar.account_claimables
(
    account   FixedString(32),
    key_xdr   String,
    ledger     UInt32,
    close_time DateTime64(3, 'UTC'),
    entry_xdr  String CODEC(ZSTD(3))
)
ENGINE = ReplacingMergeTree(ledger)
ORDER BY (account, key_xdr);

CREATE TABLE IF NOT EXISTS stellar.claimable_removals
(
    key_xdr String,
    ledger  UInt32
)
ENGINE = ReplacingMergeTree(ledger)
ORDER BY key_xdr;

CREATE TABLE IF NOT EXISTS stellar.asset_entry_ledgers
(
    asset      String,
    entry_type LowCardinality(String),
    ledger     UInt32,
    n_created  UInt32,
    n_updated  UInt32,
    n_removed  UInt32
)
ENGINE = ReplacingMergeTree
PARTITION BY intDiv(ledger, 10000000)
ORDER BY (asset, entry_type, ledger);

-- ── trades_by_account — PG trades re-keyed account-first (ADR-0048 serve-by-query-shape) ──
-- One row per (account, trade): the taker, plus the maker when it is a different account.
-- Populated by `ch-rebuild -write -sdex -trades-by-account` (no CH trades table to hang an MV on);
-- each completed window lands in trades_by_account_windows, which is the coverage evidence
-- GET /v1/accounts/{g}/trades reads before serving below the PG compression horizon.
CREATE TABLE IF NOT EXISTS stellar.trades_by_account
(
    account      String,
    ts           DateTime64(3, 'UTC'),
    ledger       UInt32,
    tx_hash      String,
    op_index     UInt32,
    source       LowCardinality(String),
    role         LowCardinality(String),
    counterparty String DEFAULT '',
    base_asset   String,
    quote_asset  String,
    base_amount  Int128,
    quote_amount Int128,
    usd_volume   String DEFAULT '',
    routed_via   String DEFAULT '',
    ingested_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (account, ts, ledger, tx_hash, op_index, source);

CREATE TABLE IF NOT EXISTS stellar.trades_by_account_windows
(
    from_ledger UInt32,
    to_ledger   UInt32,
    from_ts     DateTime('UTC'),
    to_ts       DateTime('UTC'),
    rows        UInt64,
    -- Sorted comma-joined trade sources the window decoded; the API counts only windows matching its current set.
    sources     String DEFAULT '',
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (from_ledger, to_ledger);
