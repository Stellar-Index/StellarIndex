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

-- Per-op NON-SOURCE participants (ADR-0038 Phase B). One row per
-- (account, operation) where `account` is a G-strkey the op TOUCHES but
-- did not source: a payment destination, trustor, merge target, clawback
-- victim, etc. Derived in the Go extract via xdrjson.ParticipantAccounts
-- (decodes the op body's G-strkey fields); the op's own source stays in
-- stellar.operations.source_account. Account history (GET
-- /v1/accounts/{g}/operations|transactions) is then the UNION of
-- operations.source_account = G (sourced) and a lookup here (incoming).
--
-- ORDER BY (account, …) so a per-account lookup is a primary-key range
-- scan — `account` is the sort prefix, so no separate skip-index is
-- needed (unlike operations.source_account, which is a non-prefix column
-- and therefore carries a bloom index). Live ingest fills this going
-- forward; the historical re-derive over the full op history is a
-- (multi-day, operator-gated) ch-backfill, like the Phase-C entry-change
-- fill.
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
    -- Stroop balance for account (native) + trustline entries, 0 otherwise.
    -- Lets top-holder / account-balance reads sort + aggregate in SQL without
    -- decoding every entry's XDR.
    balance      Int64 DEFAULT 0,
    ingested_at  DateTime DEFAULT now(),
    INDEX idx_lec_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lec_asset asset TYPE bloom_filter(0.01) GRANULARITY 1,
    -- Point lookups of a specific contract_data / ledger-entry key
    -- (ADR-0039 Blend reserve reads, wasm-hash + code-history readers).
    -- key_xdr is NOT in the sort key, so `WHERE key_xdr = ? / IN (…)`
    -- would full-scan ~1.7B rows; the bloom prunes granules. MATERIALIZE
    -- INDEX backfills existing parts (heavy mutation; run off-peak).
    INDEX idx_lec_key_xdr key_xdr TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index, change_index);

-- Current-state projection of ledger_entry_changes: the LATEST entry per
-- (entry_type, key) — ReplacingMergeTree(ledger_seq) keeps the highest-ledger
-- row, FINAL forces read-time dedup. Backs the account-state + asset-holder
-- reads (ADR-0038 Phase C): instead of a GROUP BY over all of an account's /
-- asset's historical changes (which grows unboundedly with the backfill), a
-- read touches ~1 row per live entry via the account_id / asset skip-indexes.
-- Kept current by the materialized view below — every insert into
-- ledger_entry_changes (live-capture + ch-backfill re-derive) flows through.
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
    INDEX idx_lecur_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lecur_asset asset TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ledger_seq)
ORDER BY (entry_type, key_xdr);

-- Feeds ledger_entries_current from every ledger_entry_changes insert.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ledger_entries_current_mv
TO stellar.ledger_entries_current AS
SELECT entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr
FROM stellar.ledger_entry_changes;

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

-- ── contract_events_daily — pre-aggregated per-contract activity ──────────
-- Serves /v1/protocols/{name} detail (event breakdown + activity series)
-- without the ~15s raw scan (BACKLOG #43 / page-audit REMAINING). The
-- source table is ReplacingMergeTree, so a SummingMergeTree MV would
-- OVERCOUNT on duplicate inserts (live-sink retries, ch-rebuild
-- re-derives re-inserting parts) — the `events` column has to dedup on
-- the event's natural key (ledger_seq, tx_hash, op_index, event_index),
-- not just sum row counts.
--
-- events uses uniqCombined(17), NOT uniqExact (2026-07-09 incident /
-- docs/architecture/contract-events-daily-redesign.md). uniqExact's state
-- is a literal hash SET that grows ~16 bytes per distinct event and is
-- UNBOUNDED for a hot contract+day+event_type+topic group — on r1 this
-- grew large enough that background merges of the AggregatingMergeTree
-- exceeded the kernel commit budget (vm.overcommit_memory=2) and
-- retry-looped, starving the live sink for hours. uniqCombined(17) hashes
-- the SAME natural key into a bounded HyperLogLog-family sketch (~10-96KB
-- per state regardless of cardinality — measured, not theoretical; see
-- the redesign doc), so it (a) still dedupes duplicate/retried natural
-- keys exactly at the cardinalities this table actually sees, avoiding
-- the same overcount SummingMergeTree would have caused, while (b)
-- merging in bounded memory. Accuracy loss is ~0.1-0.5% at the
-- cardinalities measured (500K-4M uniques/state) — this table is a
-- dashboard pre-aggregation (explorer's compact-formatted "events · 24h" /
-- event-breakdown charts), never the ADR-0033 completeness oracle, so the
-- tradeoff is one-sided: it fixes an active production fuse for
-- imperceptible display-rounding error. See the redesign doc's reader
-- survey for the full evidence chain.
--
-- Historical fill (run ONCE after creating, off-peak, windowed by
-- ledger_seq on a large existing lake — see the redesign doc's runbook
-- for the run-heavy-job-wrapped windowed form):
--   INSERT INTO stellar.contract_events_daily
--   SELECT toDate(close_time) AS day, contract_id, event_type,
--          topic_0_sym, if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
--          if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
--          uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index)
--   FROM stellar.contract_events FINAL
--   GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;
--
-- Changing the table's shape on an EXISTING deployment (r1) — an
-- AggregateFunction column's serialized state format is tied to its
-- declared function+params, so neither the t0_xdr column (2026-06, it's
-- in the ORDER BY) nor the uniqExact→uniqCombined engine swap
-- (2026-07-09) can be a bare ALTER; both needed recreate + re-fill:
--   RENAME TABLE stellar.contract_events_daily TO stellar.contract_events_daily_old;
--   -- (also drop/recreate the _mv), run this CREATE, then the fill above,
--   -- then DROP the _old table. Until the new table is populated the fast
--   -- query errors and ProtocolEventBreakdown/ProtocolDailyActivity
--   -- gracefully fall back to the raw scan.
-- The uniqCombined swap ships as a side-by-side v2 build so r1 never runs
-- with the fast path down — see
-- deploy/clickhouse/contract_events_daily_v2.sql and
-- docs/architecture/contract-events-daily-redesign.md for the exact,
-- tested apply sequence (this file's canonical CREATE below is what a
-- FRESH deployment gets automatically; r1 needs the v2 runbook because
-- IF NOT EXISTS is a no-op against its already-existing v1 table).
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

-- ── tx_hash_index — hash-ordered transaction lookup (perf-todo §4) ────────
-- GET /v1/tx/{hash} resolution table. stellar.transactions is ORDER BY
-- (ledger_seq, tx_index); its tx_hash bloom skip-index PRUNES but cannot
-- SEEK — at 10.2B rows a point lookup still scans ~96M residual rows
-- (~5.4s). This table is ORDER BY tx_hash, so hash → ledger_seq is a
-- primary-key binary search (µs); the reader then re-reads the summary
-- row ledger-scoped (partition-pruned, sub-100ms).
--
-- ReplacingMergeTree: duplicate inserts (live-sink retries, ch-backfill /
-- ch-rebuild re-derives re-inserting ranges, overlapping backfill windows)
-- collapse on merge — tx hashes are unique network-wide, so ORDER BY
-- tx_hash is the row's natural identity. The MV indexes every NEWLY
-- ingested transaction immediately; existing history needs the one-time
-- windowed operator backfill (resumable; see the ClickHouse-log/root-fill
-- caution in docs/operations/perf-todo.md §4):
--
--   stellarindex-ops ch-txindex-backfill -ch-addr 127.0.0.1:9300 \
--     -from 2 -to <lake tip> -window 5000000
--
-- The reader (ExplorerReader.TransactionByHash) falls back to the bloom
-- scan on an index MISS, so lookups stay correct while the backfill is
-- incomplete — pre-backfill hashes are just still slow.
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

-- ── account_movements — ADR-0048 D2 feed-shaped account-activity archive ──
-- Amends ADR-0047 D1 (which planned a Postgres `classic_movements` hypertable,
-- migration 0105 — applied but left UNPOPULATED, see that migration's row in
-- migrations/README.md): "serve by query shape, not by data age." The one
-- genuinely archive-scale story here — "enter an address, see everything it
-- has ever done" — is `WHERE address = X ORDER BY ledger` over what will
-- become 10-20B immutable rows; that is a ClickHouse-shaped read, not a
-- Postgres one (ADR-0048 §Context). This table is NOT a raw-lake table (it is
-- decoder-DERIVED, unlike every table above it in this file) — it is a
-- dedicated SERVING table per ADR-0048 D1, populated by
-- `stellarindex-ops classic-movements-backfill` (internal/sources/
-- classicmovements decodes `stellar.operations`/`operation_results`/
-- `ledger_entry_changes` above; internal/storage/clickhouse's
-- FanOutAccountMovement + InsertAccountMovements write here). Never written by
-- the dual-sink / live extractor.
--
-- Feed-shaped: TWO rows per movement (one per participant), with a
-- `direction` discriminator (sent/received/self) rather than one row with
-- from/to columns — so a single-address query needs no OR / UNION. Row
-- cardinality per movement_kind (mirrors internal/sources/classicmovements'
-- exact FromAddress/ToAddress decode semantics — see that package's doc.go
-- and README.md for the full per-op derivation):
--   payment / create_account / path_payment / clawback / account_merge
--     -> 2 rows (from_address != to_address, both known: one 'sent' row for
--        the source, one 'received' row for the destination)
--   payment, degenerate self-payment (from_address == to_address)
--     -> 1 row, direction='self' (never sent+received for the same address —
--        see FanOutAccountMovement's doc)
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
--   liquidity_pool_withdraw (CAP-0038 auto-liquidation edge, Phase 4,
--   attributes.revocation=true)
--     -> 1 row per created ClaimableBalanceEntry (trustor known, destination
--        escrow unknown) -- 2 for a real liquidation (every classic AMM pool
--        has exactly two assets)
--
-- Engine: ReplacingMergeTree(ingested_at), same idempotent-re-derivation
-- convention as every table above (see this file's header) — re-running an
-- already-written classic-movements-backfill window is a safe no-op once
-- merges settle.
--
-- ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction) is
-- ADR-0048 D2's exact key: `address` first makes a per-account read a
-- contiguous primary-key range scan (the entire point of this table existing
-- instead of a Postgres `WHERE address = ?` over an unindexed-at-that-scale
-- hypertable); the remainder is the row's natural unique identity within one
-- account's feed. `direction` is the LAST key column deliberately: the
-- sent/received pair from one movement lands at two DIFFERENT `address`
-- values (no collision risk from direction there), so it exists purely to
-- keep a 'self' row from colliding with the address's own past/future
-- sent/received rows at the same (ledger, tx_hash, op_index, leg_index).
--
-- amount: Int128, matching stellar.supply_flows' sibling convention. Classic
-- amounts are int64-stroop-scale and are NOT special-cased (ADR-0047 D1) —
-- Int128 costs nothing extra per row and keeps every amount column in the
-- lake/serving tier uniformly wide, avoiding a second amount-typing
-- convention for one table.
--
-- attributes: JSON-as-String (not a native JSON/Map type), mirroring
-- migration 0105's `attributes jsonb` remainder 1:1 (balance_id, claimants,
-- send_asset/send_amount, pool_id, revocation, …) — read via
-- JSONExtractString/JSONExtract at query time, never a SQL predicate target
-- in the hot path here (FindClaimableBalanceCreates' balance_id lookup is the
-- one exception, backed by idx_cb_balance_id below — see that function's doc
-- comment for the 2026-07-12 full-scan finding that motivated it).
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
    -- 2026-07-12 finding: classic-movements-backfill's Phase-3 claimable-balance
    -- fallback (clickhouse.FindClaimableBalanceCreates) was a 6.5s full scan of
    -- 973M rows PER lookup during the claimable-balance-bot era (ledgers
    -- ~34M-40M, thousands of refs per window) before this index existed; the
    -- bloom skip-index brought a single lookup to ~84ms (~77x). Only prunes when
    -- the WHERE predicate is textually IDENTICAL to this expression.
    INDEX idx_cb_balance_id JSONExtractString(attributes, 'balance_id') TYPE bloom_filter(0.01) GRANULARITY 4,
    -- BACKLOG #72 (2026-07-16): structural fix for `/v1/accounts/{g}/movements`
    -- timing out on extreme-volume addresses (e.g. an airdrop-sink address with
    -- 264M received payments) — the reverse keyset read (`WHERE address = ?
    -- ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC LIMIT
    -- ?`) has to fan out across every one of this table's ~473 ledger-range
    -- PARTITION BY partitions the address ever touched (~140 of them for the
    -- pathological address): an equality filter on `address` prunes WITHIN a
    -- touched partition (address already leads this table's own ORDER BY) but
    -- cannot prune ACROSS partitions, since partitioning is by ledger range,
    -- not by address. A projection can't declare its own PARTITION BY (it's
    -- always co-partitioned with its parent table), so this isn't a sort-order
    -- fix — its win is forcing a deterministic, one-time full compaction of the
    -- touched partitions' parts (vs. today's fragmentation, hostage to
    -- background-merge cadence contending with Phase-0's concurrent
    -- genesis-extension writes into old partitions — see the ROADMAP's "partly
    -- self-heals as parts merge post-Phase-0" note) plus a narrower column list
    -- (drops `ingested_at`, which the reader never selects) that shrinks bytes
    -- moved per touched partition. Same retrofit caveat as idx_cb_balance_id
    -- above: `CREATE TABLE IF NOT EXISTS` does NOT retrofit a projection onto
    -- an already-existing table — this only takes effect on a fresh install.
    -- Applying it to r1's existing 6.76B-row table needs `ALTER TABLE ... ADD
    -- PROJECTION` + `MATERIALIZE PROJECTION`, a HEAVY operation deliberately
    -- NOT run by this schema file — see
    -- docs/operations/runbooks/account-movements-projection.md for the
    -- wrapped procedure + the Phase-0-free-window precondition.
    PROJECTION proj_by_address
    (
        SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
               movement_kind, provenance, asset, counterparty, amount, attributes
        ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)
    )
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction);
