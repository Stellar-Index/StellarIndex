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
    -- Soroban resource metering (extract.go:extractSorobanMetering). All
    -- DEFAULT 0 → additive + old-binary-safe; only Soroban txs are non-zero, so
    -- these sparse columns compress to near-nothing. DECLARED = the submitter's
    -- resource bid (tx envelope SorobanTransactionData); the three *_fee values
    -- are what core CHARGED (tx meta SorobanTransactionMetaExtV1). There is no
    -- actual-instructions-consumed value in pubnet ledger meta, so none is
    -- stored. Populated GO-FORWARD only; historical Soroban txs read 0.
    soroban_instructions      UInt32 DEFAULT 0,  -- declared CPU instruction bid
    soroban_disk_read_bytes   UInt32 DEFAULT 0,  -- declared
    soroban_write_bytes       UInt32 DEFAULT 0,  -- declared
    soroban_read_entries      UInt16 DEFAULT 0,  -- declared len(footprint.ReadOnly)
    soroban_write_entries     UInt16 DEFAULT 0,  -- declared len(footprint.ReadWrite)
    soroban_resource_fee_bid  Int64  DEFAULT 0,  -- declared total resource-fee bid
    soroban_nonrefundable_fee Int64  DEFAULT 0,  -- actual non-refundable fee charged
    soroban_refundable_fee    Int64  DEFAULT 0,  -- actual refundable fee charged
    soroban_rent_fee          Int64  DEFAULT 0,  -- actual rent fee charged
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
    topics_xdr         Array(String) CODEC(ZSTD(3)),
    data_xdr           String,
    op_args_xdr        Array(String) CODEC(ZSTD(3)),
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
    -- CODEC(ZSTD(3)) applied on R1 2026-07-18/19 (Phase A capacity relief,
    -- docs/operations/runbooks/phase-a-capacity-relief-2026-07-18.md Step 2)
    -- — measured 1.75x over the LZ4 default on entry_xdr. Keep this in sync
    -- with the live ALTER so schema and new-ingest match (audit-2026-07-23
    -- DAT-04: this had drifted).
    key_xdr      String CODEC(ZSTD(3)),
    entry_xdr    String CODEC(ZSTD(3)),
    -- ingested_at sits HERE, before the ADR-0038 columns below, because that
    -- is the LIVE column order: account_id/asset/balance/intra_ledger_seq
    -- were ALTER TABLE ADDed on r1 (appended after ingested_at), and the
    -- drift check compares ordered column lists (2026-08-24: this file
    -- declared them inline and read as .columns drift forever). A fresh
    -- bootstrap from this file now matches the operative table exactly.
    ingested_at  DateTime DEFAULT now(),
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
    -- Position of this change within its ledger's canonical entry-change walk
    -- — a per-LEDGER monotonic counter (unlike change_index, which restarts
    -- per transaction), assigned in LEDGER-WIDE PHASE order:
    --   phase 1  every tx's fee changes            (tx-set apply order)
    --   phase 2  every tx's apply-phase meta       (tx-changes-before, per-op
    --                                               changes in op_index /
    --                                               change_index order,
    --                                               tx-changes-after)
    --   phase 3  every tx's post-apply fee changes (P23 Soroban refunds)
    -- This mirrors the SDK's canonical ingest.LedgerChangeReader state machine
    -- (feeChangesState → metaChangesState → postTxApplyState) and
    -- dispatcher.walkLedgerEntryChanges. This comment previously described a
    -- PER-TRANSACTION order ("within each tx: fee-changes, …"), which ranked
    -- tx1's apply-phase change BELOW tx2's fee change and so let FINAL keep a
    -- fee-phase row as a key's ledger-final state (C2-032, audit-2026-07-23).
    -- Folded into ledger_entries_current's ReplacingMergeTree version so the
    -- LAST change to a key within one ledger wins FINAL dedup
    -- deterministically (audit-2026-07-16 C2-4c). DEFAULT 0 (old-binary-safe
    -- + the value legacy/pre-fix rows carry until a re-derive repopulates
    -- them); snapshot/seed backfill rows stamp 4294967295 (math.MaxUint32 —
    -- authoritative final state for their ledger).
    --
    -- POSITIONS ARE SCOPED TO dispatcher.EntryWalkVersion (currently 2).
    -- The C2-032 fix RENUMBERED every ledger, so a legacy row can carry a
    -- HIGHER version than the corrected row for the same key — and a lower
    -- RMT version never displaces a higher one. Re-deriving a range therefore
    -- requires DROPPING the partition first, not just re-inserting. See
    -- migrations/0120 and docs/operations/runbooks/entry-walk-renumbering.md.
    intra_ledger_seq UInt32 DEFAULT 0,
    INDEX idx_lec_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lec_asset asset TYPE bloom_filter(0.01) GRANULARITY 1,
    -- Point lookups of a specific contract_data / ledger-entry key
    -- (ADR-0039 Blend reserve reads, wasm-hash + code-history readers).
    -- key_xdr is NOT in the sort key, so `WHERE key_xdr = ? / IN (…)`
    -- would full-scan the whole table; the bloom prunes granules. MATERIALIZE
    -- INDEX backfills existing parts (heavy mutation; run off-peak).
    --
    -- FP rate 0.0001, NOT the 0.01 default this index shipped with
    -- (audit-2026-07-23 C-F1). A bloom's FP rate is a pruning FLOOR: it
    -- cannot survive fewer than FP x total granules however selective the
    -- key is. Measured on r1 2026-07-24, `EXPLAIN indexes=1` for the
    -- ContractCodeHistory query shape over this table at 150.78B rows /
    -- 6.17 TiB — 18,405,620 granules total, 172,942 surviving the index
    -- (0.94% == the index's own FP rate), 1.38B read_rows / 101.58 GiB /
    -- 21,538 ms to return FOUR rows. Every
    -- /v1/contracts/{id}/code-history request therefore blew the 8s
    -- explorer read deadline and 500'd, deterministically, for every
    -- contract. No query rewrite beats the floor; only the FP rate does.
    -- At 1e-4 the floor drops ~100x to ~1,840 granules ≈ 15M rows
    -- (~2x that for the reader's 2-key IN-list, since the FP applies per
    -- probed value) — comfortably inside the deadline.
    --
    -- COST, stated honestly: ClickHouse sizes a bloom granule as
    -- bits_per_row x DISTINCT values in the granule, and
    -- BloomFilterHash::calculationBestPractices maps 0.01 -> ~10 bits/row
    -- and 1e-4 -> the 20 bits/row ceiling, so this index roughly DOUBLES
    -- in bytes. Measure before materializing on a constrained pool —
    -- deploy/clickhouse/ledger_entry_changes_key_xdr_index_fp.sql has the
    -- system.data_skipping_indices query, the per-partition procedure and
    -- the 0.001 fallback. (1e-4 is near the implementation floor: 20
    -- bits/row + 15 hashes bottoms out around 6.7e-5, so asking for less
    -- silently buys nothing.) idx_lec_account_id / idx_lec_asset are
    -- deliberately LEFT at 0.01 — same floor, but their readers degrade to
    -- latency rather than errors and each retune costs the same doubling
    -- on a pool that was at 92% when this was written.
    INDEX idx_lec_key_xdr key_xdr TYPE bloom_filter(0.0001) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY (ledger_seq, tx_hash, op_index, change_index);

-- Current-state projection of ledger_entry_changes: the LATEST entry per
-- (entry_type, key) — ReplacingMergeTree keeps the highest-VERSION row, FINAL
-- forces read-time dedup. Backs the account-state + asset-holder reads
-- (ADR-0038 Phase C): instead of a GROUP BY over all of an account's / asset's
-- historical changes (which grows unboundedly with the backfill), a read
-- touches ~1 row per live entry via the account_id / asset skip-indexes. Kept
-- current by the materialized view below — every insert into
-- ledger_entry_changes (live-capture + ch-backfill re-derive) flows through.
--
-- Version = (ledger_seq << 32) | intra_ledger_seq, NOT ledger_seq alone
-- (audit-2026-07-16 C2-4c). ledger_seq is not unique per key within a ledger —
-- a single ledger can hold several changes to one key (update-then-remove, or
-- remove-then-recreate). With ledger_seq as the sole version those same-ledger
-- rows tied, so FINAL kept an ARBITRARY one: it could resurrect a deleted entry
-- (keep a stale before-image over a later 'removed') or serve a mid-ledger
-- state. Folding the per-ledger intra_ledger_seq (the canonical within-ledger
-- walk position — see ledger_entry_changes.intra_ledger_seq) into the low 32
-- bits makes the version strictly monotonic in canonical order, so FINAL
-- deterministically keeps the LAST change (including a removal). The high 32
-- bits are the ledger, so cross-ledger ordering is unchanged. version is
-- MATERIALIZED (computed on insert, never written by a client) — the MV and any
-- reproject INSERT supply only the base columns.
--
-- NOTE: intra_ledger_seq is 0 on every row written before this fix (the column
-- DEFAULT) and on legacy rows until a full re-derive of ledger_entry_changes
-- repopulates it; for those rows same-ledger ties remain unbroken exactly as
-- before (no regression). The tie-break is effective for all NEW ingest
-- immediately, and for historical ledgers once re-derived. Existing deployments
-- reproject this table from ledger_entry_changes via the operator-run migration
-- deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql (a fresh deploy
-- gets the corrected shape straight from this file).
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

-- Slim TTL-liveness projection: key_hash → liveUntilLedgerSeq (v0.21.4). Backs
-- internal/storage/clickhouse/ttl_liveness.go's ClassifyTTLLiveness as a
-- primary-key lookup, replacing the per-batch scan of ledger_entries_current's
-- 586M ttl rows (which read the wide entry_xdr per row and OOM'd its own 8 GiB
-- pin — six failed production attempts, 2026-07-29). Three tiny columns, one
-- row per TTL change: ~20-30 GB vs the 590 GB source; the reader is bounded by
-- construction. The v0.21.4+ binary hard-errors if this table is absent — no
-- scan fallback exists.
--
-- Extraction (production-validated 2026-07-28; ttl_liveness_test.go pins these
-- expressions against the Go layout constants, here AND in the operator
-- migration artifact deploy/clickhouse/ttl_live_until.sql — keep all three in
-- lockstep): a TTL LedgerKey is 36 bytes (type=00000009 | sha256(LedgerKey)),
-- a TTLEntry is 48 bytes (lastModified(4) | data.type(4) | keyHash(32) |
-- liveUntilLedgerSeq(4) | ext(4)); XDR is big-endian, reinterpretAsUInt32 is
-- little-endian, hence reverse(). version = (ledger_seq << 32) |
-- intra_ledger_seq — same composite as ledger_entries_current (C2-4c).
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
    INDEX idx_cb_balance_id JSONExtractString(attributes, 'balance_id') TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction);

-- ── ops_by_source: slim sourced-history projection (2026-07-30) ─────────────
-- (source_account → ledger/tx/op keys) from BOTH stellar.operations (op-
-- effective source, real op_index) and stellar.transactions (tx source,
-- sentinel op_index 4294967295). The account-history readers' sourced arms
-- read this via PK prefix instead of the source_account bloom probe over the
-- full 23B/34B-row tables (measured 6.17s → PK-shaped; explorer_reader.go).
-- The v0.21.7+ binary REFUSES account-history reads without this table — no
-- scan fallback. Keep in lockstep with the operator migration artifact
-- deploy/clickhouse/ops_by_source.sql (which also carries the Step-2
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

-- ── account_activity — see deploy/clickhouse/account_activity.sql (#31) ──
-- Per-account activity watermark: (account → the max ledger the account was
-- EVER named in, across EVERY role the account-history readers serve — op
-- source, tx source/fee-payer, non-source participant). ~1 row per account,
-- tiny vs the multi-billion-row lake tables. AccountOperations reads
-- max(last_ledger) as an EXACT UPPER BOUND (`ledger_seq <= ?`) on its per-arm
-- reverse primary-key resolves, so a long-idle account's page stops at the
-- account's real last activity instead of walking granules back from the tip
-- (measured ~4s live for a 46d-idle account, 2026-08-24).
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
-- back to the UNBOUNDED scan when an account has no row (pre-backfill
-- accounts degrade to the old perf, never to missing rows).
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
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY asset;

CREATE TABLE IF NOT EXISTS stellar.asset_holders_counts_staging
AS stellar.asset_holders_counts;

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
-- contract_instance_changes — per-contract instance-executable timeline
-- index for the explorer's code-history + wasm reads (open-fixes
-- inventory #26 items 3 + wasm; route sweeps 2026-07-29 → 2026-08-09:
-- /v1/contracts/{id}/code-history was the LAST persistent 503 class).
--
-- WHY: stellar.ledger_entry_changes is ORDER BY (ledger_seq, …), so the
-- "instance entry for contract X" predicate (key_xdr IN (…)) is
-- scan-shaped over the whole changes history — 8s+ cold and 503 under
-- the explorer budget for any non-prewarmed contract. Both
-- ContractCodeHistory and the ContractWasm hash resolution pay it.
--
-- THE INDEX: one narrow row per captured instance-entry WRITE, keyed
-- (contract, ledger, change_index), carrying just the decoded verdict:
-- SAC or wasm + which hash. The wire layout makes the MV extraction a
-- pair of fixed-offset substrings (byte-verified 2026-08-09 against
-- go-stellar-sdk marshalling, all three shapes):
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
-- Instance-STORAGE writes rewrite the same key (audit-2026-07-23 C-F1),
-- so a busy contract contributes many rows — but each is ~90 bytes vs
-- the multi-KB entry_xdr, and readers collapse to distinct executables.
--
-- REPLAY-SAFE: no counts; identical (contract, ledger, change_index)
-- keys re-inserted by re-derives/overlapping backfills collapse in the
-- RMT (the migration-0059 double-count class does not apply).
--
-- OPERATOR CONTRACT (same as contract_active_ledgers): presence +
-- non-empty = readers TRUST it, including per-contract emptiness
-- ("instance never captured"). Apply this DDL, then IMMEDIATELY run the
-- windowed historical backfill to genesis — serialized with other heavy
-- jobs:
--
--   /usr/local/sbin/run-heavy-job.sh instance-changes-backfill \
--     /usr/local/bin/stellarindex-ops ch-instance-backfill \
--     -ch-addr 127.0.0.1:9300 -from 2 -window 2000000
--
-- Do NOT leave the table applied-but-unbackfilled on a lake with
-- history: cold contracts would resolve "no wasm" / truncated upgrade
-- timelines stamped as complete.

CREATE TABLE IF NOT EXISTS stellar.contract_instance_changes
(
    contract_hash FixedString(64),  -- lower-hex 32-byte contract id
    ledger_seq    UInt32,
    change_index  UInt32,
    close_time    DateTime('UTC'),
    is_sac        UInt8,            -- executable type: 0 = wasm, 1 = SAC
    wasm_hash     String,           -- lower-hex, '' when is_sac = 1
    ingested_at   DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (contract_hash, ledger_seq, change_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_instance_changes_mv
TO stellar.contract_instance_changes AS
SELECT
    lower(hex(substring(tryBase64Decode(key_xdr), 9, 32)))  AS contract_hash,
    ledger_seq,
    change_index,
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
-- contracts_census_daily — day-keyed per-contract event counts behind
-- the /v1/contracts directory (open-fixes inventory #26 item 2).
--
-- WHY: the directory's census query (uniqExact over the PK of
-- billions-row contract_events, GROUP BY contract_id over multi-day
-- windows) measured 40s / 790M rows per run and ran ~160 times per 3h
-- across the prewarm rungs. Ranking from contract_events_daily was
-- MEASURED WORSE (24.5s — merging ~15M uniqCombined HLL states), and
-- contract_active_ledgers is contract-first so a window scan doesn't
-- prune. The durable shape is plain per-day counts computed ONCE per
-- day.
--
-- DUP-SAFETY (the migration-0059 Summing-MV double-count class): this
-- table has NO MV. A periodic job (stellarindex-ops ch-census-rollup)
-- recomputes whole days and swaps them in atomically with
-- ALTER TABLE … REPLACE PARTITION — recomputing a day is idempotent by
-- construction, and a partial write never mixes with served rows.
-- Completed days are written once; the current (partial) day is
-- re-replaced every cycle, so the serving tail is at most one cadence
-- (30 min) stale — inside the directory's SWR staleness budget.
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
    -- uniqExact over (ledger_seq, tx_hash, op_index, event_index) —
    -- exact, matching the legacy census (audit: count() over-counted
    -- via lake duplicate rows).
    events      UInt64,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = MergeTree
PARTITION BY day
ORDER BY (day, contract_id);

CREATE TABLE IF NOT EXISTS stellar.contracts_census_daily_staging
AS stellar.contracts_census_daily;
