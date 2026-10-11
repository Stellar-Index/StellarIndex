-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically — the gate named below pins that,
-- so the DDL here is a mirror and the runbook is the reason the file exists.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- account_creators rollup — the "who bootstrapped the most accounts"
-- league table behind GET /v1/accounts/creators.
--
-- SOURCE, BELOW PROTOCOL 23: stellar.account_movements (ADR-0048 D2). A
-- `create_account` lands as two rows, direction='sent' on the funder and
-- 'received' on the created account, with `counterparty` the other side and
-- `amount` the starting balance in stroops. The funder arm alone is one
-- successful CreateAccount per row, already decoded; nothing is read from the
-- undecoded stellar.operations.body_xdr.
--
-- SOURCE, AT OR ABOVE PROTOCOL 23: that archive is historical-only (ADR-0047
-- D2; classic-movements-backfill clamps -to below ledger 58,762,517) because
-- CAP-67 folded classic payments into the token-event model. The funding leg
-- is then a `transfer` movement written by the live ch-cap67-movements daemon,
-- and the OperationTypeCreateAccount row in stellar.operations, joined on the
-- full (ledger, tx_hash, op_index) identity, says it was a creation.
--
-- Reading only the classic arm would end the population at the boundary. The
-- arms clamp on opposite sides of one constant (clickhouse.P23BoundaryLedger,
-- pinned by TestP23BoundaryConstantsAgree), so their union is every ledger and
-- their intersection is empty.
--
-- The pairing goes THROUGH the movement because stellar.operations retains
-- failed transactions' operations by design: a CreateAccount in a failed
-- transaction matches no transfer leg, so the join is also the success gate
-- and the operations side contributes only the join key. The movement's
-- `address` equals the operation's source, `counterparty` is the created
-- G-strkey and `asset` is native; zero amounts are real (CAP-33 sponsored
-- creation).
--
-- WHY A ROLLUP. The predicate is movement_kind, which is not in
-- account_movements' ORDER BY, so finding creations scans the whole archive: a
-- cycle cost, not a request cost. Same staging + atomic EXCHANGE shape as
-- asset_holders_rollup / accounts_stats; readers are keyed and tiny.
--
-- WHY THE CYCLE WALKS PARTITIONS. As one statement, the dedupe hash table (one
-- state per creation in all history) and the LEFT JOIN build side (one row per
-- existing account) are sized by different growing populations and together
-- exceeded the ops-batch class's 8 GiB budget, so the endpoint served 503.
-- Raising the ceiling only moves which cycle fails.
--
-- So the archive pass is WALKED, one 1M-ledger partition per statement, into
-- stellar.account_creators_ops, and the board's join runs once against that
-- working table. Widest window is 701 MiB at max_threads=2, against 3.31 GiB
-- for the single join. The post-P23 arm keeps a join of its own, bounded by
-- the window (peak 216 MiB-1.43 GiB).
--
-- Each arm is free on windows the other owns: the boundary clamp is a
-- predicate on both tables' partition key, so a window wholly on the far side
-- prunes to no parts.
--
-- Nothing is written to a staging arm until the walk has finished, so an
-- interrupted cycle leaves the previous board live and the single EXCHANGE is
-- the only moment anything becomes visible.
--
-- COVERAGE IS DATA-DERIVED (ADR-0031). The cycle records the ledger span it
-- actually aggregated (min/max ledger and close time over both arms) into
-- account_creators_stats; nothing assumes genesis or substitutes the tip, and
-- the API serves the span verbatim.
--
-- SCOPE: the CREATOR relationship only (funder -> created account, immutable
-- history). The SPONSOR relationship has a different source
-- (LedgerEntry.ext.v1.sponsoringID inside entry_xdr, not projected) and is
-- deliberately absent rather than approximated.

-- Narrow, deduplicated projection of every account creation, written one lake
-- partition at a time by both arms. Not served; it gives the walked passes a
-- bounded landing place so the join against live account entries is paid ONCE
-- per cycle. stellar.account_movements is a ReplacingMergeTree, so duplicates
-- are collapsed over its full ORDER BY key; the partition expression is a
-- function of `ledger`, part of that key, so per-window grouping is exact.
-- Partitioned to mirror the walk, ordered by creator as the board arm reads it
-- back.
CREATE TABLE IF NOT EXISTS stellar.account_creators_ops
(
    creator   String,
    created   String,
-- Starting balance in stroops; Int128 to match account_movements.amount.
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
-- Sum of starting balances, stroops. Int128: a sum over all history has no
-- a-priori Int64 bound; served as a decimal string (ADR-0003).
    funded_stroops   Int128,
-- Point-in-time, unlike the columns above: the created set still existing as
-- an account entry, and the XLM it holds. Accounts merge away and balances
-- move.
    live_accounts    UInt64,
    live_stroops     Int128,
    first_ledger     UInt32,
    last_ledger      UInt32,
    first_created_at DateTime('UTC'),
    last_created_at  DateTime('UTC'),
    computed_at      DateTime DEFAULT now(),
-- ORDER BY rank serves the top-N page; the per-creator read filters on
-- creator, which without this index reads every granule.
    INDEX idx_creators_rollup_creator creator TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
ORDER BY rank;

CREATE TABLE IF NOT EXISTS stellar.account_creators_rollup_staging
AS stellar.account_creators_rollup;

-- IF NOT EXISTS keeps an older board's definition, so add the index to both
-- halves of the EXCHANGE pair. No MATERIALIZE INDEX: every cycle TRUNCATEs
-- staging and rewrites the whole board.
-- board, so the first cycle after this lands an indexed live table.
ALTER TABLE stellar.account_creators_rollup
    ADD INDEX IF NOT EXISTS idx_creators_rollup_creator creator TYPE bloom_filter(0.01) GRANULARITY 1;
ALTER TABLE stellar.account_creators_rollup_staging
    ADD INDEX IF NOT EXISTS idx_creators_rollup_creator creator TYPE bloom_filter(0.01) GRANULARITY 1;

-- Metric-keyed like accounts_stats so a new figure is an INSERT, not a
-- migration. Carries the totals AND the data-derived coverage span:
--   creators_total, creations_total  — over the whole aggregation, not
--                                      just the board's top rows
--   from_ledger, thru_ledger         — min/max ledger actually read
--   from_time, thru_time             — the same bounds as unix seconds
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

-- ── Creation GRAPH edges ─────────────────────────────────────
-- The board answers "who created the most accounts"; these two tables answer
-- the graph question in both directions: whom did an account create, and who
-- created THIS account (which the board cannot serve).
--
-- ONE ROW PER DISTINCT (creator, created) PAIR, NOT PER CREATION; the collapse
-- is what bounds the inbound direction. CreateAccount and AccountMerge are
-- both repeatable and some services recycle an address continuously (one
-- address has 29,634 creations in partition 63 against matching AccountMerge
-- operations), which is recycling, not duplication. Collapsed to pairs, no
-- created account has more than 9 distinct creators; serving events would make
-- "who created this account" a 29,634-row answer.
--
-- TWO TABLES, ONE CONTENT, TWO SORT ORDERS, so each direction is a primary-key
-- range read, not a scan (the same fix class as stellar.ops_by_source). The
-- by_created twin is filled FROM the by-creator staging arm, so the second
-- ordering costs a re-sort, not a second archive pass.
--
-- WHY NOT READ stellar.account_creators_ops DIRECTLY. It is a WORKING table
-- the cycle TRUNCATEs and refills over its walked windows, so mid-cycle it
-- holds a PARTIAL archive. These two are staged and EXCHANGEd with the board,
-- so a served read never sees a half-built graph.
--
-- COST: 3.14 GiB at max_threads=2, below the cycle's peak (the board's
-- account-population join, 3.31 GiB), so the ceiling is unchanged. Headroom is
-- about 150 bytes per distinct pair: the 8 GiB budget is reached near 55 M
-- pairs (21 M when measured).
CREATE TABLE IF NOT EXISTS stellar.account_creator_edges
(
    creator        String,
    created        String,
-- creations counts the CreateAccount operations behind this edge (>1 for
-- recycling). funded_stroops sums their starting balances and is legitimately
-- 0 for CAP-33 sponsored creations.
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
