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
-- league table behind GET /v1/accounts/creators (#351).
--
-- SOURCE, BELOW PROTOCOL 23. stellar.account_movements, the feed-shaped
-- classic-movement archive (ADR-0048 D2). A `create_account` operation
-- lands there as two rows — direction='sent' on the funder and
-- direction='received' on the created account — with `counterparty`
-- carrying the other side and `amount` the starting balance in stroops.
-- The funder arm alone (direction='sent') is the creation record: one
-- row per successful CreateAccount, already decoded. Nothing has to be
-- read out of stellar.operations.body_xdr, which is undecoded base64
-- XDR.
--
-- SOURCE, AT OR ABOVE PROTOCOL 23. The same table, a different
-- representation. That archive is historical-only by design (ADR-0047
-- D2) — `classic-movements-backfill` hard-clamps its -to below ledger
-- 58,762,517 — because CAP-67 folded classic payments into the
-- token-event model at that ledger. The creation's funding leg is then a
-- `transfer` movement written by the live ch-cap67-movements daemon, and
-- what says the transfer WAS a creation is the
-- OperationTypeCreateAccount row in stellar.operations, joined on the
-- full (ledger, tx_hash, op_index) operation identity.
--
-- Reading only the classic arm ranked creators over a population that
-- ended at the boundary — 4,715,612 creations short of the tip on r1
-- 2026-09-07 (#493). The two arms clamp on opposite sides of one
-- constant (clickhouse.P23BoundaryLedger, pinned against
-- classicmovements.P23StartLedger and timescale.SEP41MovementsFloorLedger
-- by TestP23BoundaryConstantsAgree), so their union is every ledger and
-- their intersection is empty.
--
-- The pairing goes THROUGH the movement rather than reading the
-- operation's own columns because stellar.operations retains failed
-- transactions' operations by design. Measured on r1 2026-09-07 over
-- ledgers 63,000,000-63,010,000: of 6,266 distinct CreateAccount
-- operations, the 5,890 in successful transactions each match exactly
-- one transfer leg and the 376 in failed transactions match none — so
-- the join is also the success gate, and the operations side contributes
-- the join key and nothing else. On the same sample the movement's
-- `address` equals the operation's source account in every case, its
-- `counterparty` is the created G-strkey, and its `asset` is native;
-- zero amounts are real (CAP-33 sponsored creation), not missing ones.
--
-- WHY A ROLLUP. The predicate is movement_kind, which is not in
-- account_movements' ORDER BY (address, ledger, tx_hash, op_index,
-- leg_index, direction), so finding creations is scan-shaped over the
-- whole 10.31-billion-row archive. That is a cycle cost, not a request
-- cost. The same staging + atomic EXCHANGE shape as
-- asset_holders_rollup / accounts_stats: readers are keyed and tiny.
--
-- WHY THE CYCLE WALKS PARTITIONS. As one statement the aggregation held
-- two things at once whose sizes are set by different populations: the
-- dedupe hash table, one state per creation in all of history, and the
-- LEFT JOIN's build side, one row per account that currently exists.
-- Measured on r1 2026-09-06 the pair summed past the ops-batch class's
-- 8 GiB budget — 8.12 GiB in FillingRightJoinSide, with the dedupe
-- already spilled to 32 external parts and all 10,309,146,441 movement
-- rows read — and the endpoint served 503 for want of a board. Raising
-- the ceiling would only move which cycle fails, because neither
-- population stops growing.
--
-- So the archive pass is WALKED, one 1M-ledger partition per statement,
-- into stellar.account_creators_ops below, and the board's join is taken
-- OUT of the walk and run once against that working table. Measured on
-- r1 2026-09-06 at max_threads=2: the widest classic creation window
-- costs 13.9 s / 701.17 MiB (partition 55, 1,027,707 rows) and a window
-- with no creations 2.9 s / 11.27 MiB, against 3.31 GiB for the single
-- join. The post-P23 arm keeps a join of its own, but one whose build
-- side is bounded by the window and pinned rather than estimated: across
-- the seven post-P23 partitions on r1 2026-09-07 it costs 16.0-106.9 s
-- per window at a peak of 216 MiB-1.43 GiB, for 4,715,612 creations.
-- Peak is a function of one partition's creations plus the account
-- population, and both are stated rather than extrapolated; the cycle
-- goes from about 6m11s to about 13m10s and its peak is unchanged.
--
-- Each arm is free on the windows the other owns. The boundary clamp is
-- a predicate on the partition key of both tables, so a window wholly on
-- the far side prunes to no parts — measured at 0 rows read and 2-4 ms
-- per arm on r1 2026-09-07. A window is still scanned once.
--
-- Nothing is written to a staging arm until the walk has finished, so an
-- interrupted cycle leaves the previous cycle's board live and the
-- single EXCHANGE remains the only moment anything becomes visible.
--
-- COVERAGE IS DATA-DERIVED (ADR-0031). The cycle records the ledger
-- span it actually aggregated — min/max ledger and close time over the
-- creation rows it read, from both arms — into account_creators_stats.
-- Nothing assumes genesis and nothing substitutes the tip. The API
-- serves that span verbatim so the page can state what it covers instead
-- of implying the whole chain; thru_ledger reaches the tip now because
-- the board covers it.
--
-- SCOPE. This is the CREATOR relationship only: funder → created
-- account, immutable history. The SPONSOR relationship (who currently
-- pays an entry's base reserve) is a different question with a
-- different source — LedgerEntry.ext.v1.sponsoringID, which is inside
-- the base64 entry_xdr blob on stellar.ledger_entries_current and is
-- not projected as a column anywhere. It is deliberately absent here
-- rather than approximated; see #351.

-- Narrow, deduplicated projection of every account creation, written
-- one lake partition at a time, by both arms. Not served; it exists so
-- the walked passes over the archive can land their rows somewhere
-- bounded, and so the join against the live account entries is paid ONCE
-- per cycle instead of once per window. stellar.account_movements is a ReplacingMergeTree, so
-- duplicates are collapsed over its full ORDER BY key rather than
-- trusted away; the partition expression is a function of `ledger`,
-- which is part of that key, so per-window grouping is exact.
-- Partitioned to mirror the walk (one part written per window) and
-- ordered by creator, which is how the board arm reads it back.
CREATE TABLE IF NOT EXISTS stellar.account_creators_ops
(
    creator   String,
    created   String,
    -- Starting balance in stroops. Int128 to match
    -- account_movements.amount.
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
    -- Sum of starting balances, stroops. Int128 to match
    -- account_movements.amount and because a sum over the whole history
    -- has no a-priori Int64 bound; served as a decimal string
    -- (ADR-0003).
    funded_stroops   Int128,
    -- The created set that still exists as an account entry, and the
    -- native XLM it holds now. Point-in-time, unlike the two columns
    -- above: accounts merge away and balances move.
    live_accounts    UInt64,
    live_stroops     Int128,
    first_ledger     UInt32,
    last_ledger      UInt32,
    first_created_at DateTime('UTC'),
    last_created_at  DateTime('UTC'),
    computed_at      DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY rank;

CREATE TABLE IF NOT EXISTS stellar.account_creators_rollup_staging
AS stellar.account_creators_rollup;

-- Metric-keyed like accounts_stats so a new figure is an INSERT rather
-- than a schema migration. Carries the totals AND the data-derived
-- coverage span:
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

-- ── Creation GRAPH edges (#351) ─────────────────────────────────────
--
-- The board above answers "who created the most accounts". These two
-- tables answer the graph question the same issue asks, in both
-- directions: whom did an account create, and — the direction the board
-- cannot serve at all — who created THIS account.
--
-- ONE ROW PER DISTINCT (creator, created) PAIR, NOT PER CREATION, and
-- the collapse is what makes the inbound direction bounded. An account
-- can be created more than once: CreateAccount and AccountMerge are both
-- repeatable, and some services recycle an address continuously.
-- Measured on r1 2026-09-09 over lake partition 63 (ledgers
-- 63,000,000-63,999,999), 41,358 of 418,016 distinct created accounts
-- carry more than one creation row and the widest carries 29,634 — which
-- is recycling and not duplication: a sampled 20,000-ledger slice of
-- that address holds 352 creations against 352 AccountMerge operations
-- sourced by it. Collapsed to distinct pairs the same window holds
-- 419,977 edges and NO created account has more than 9 distinct
-- creators (p99.99 = 4.4). Serving creation EVENTS inbound would have
-- made "who created this account" a 29,634-row answer for a single
-- address.
--
-- Whole-archive shape measured the same day: 24,824,706 creation events
-- collapse to 20,954,070 distinct pairs.
--
-- TWO TABLES, ONE CONTENT, TWO SORT ORDERS. Each direction is then a
-- primary-key range read and neither is a scan — the same fix class as
-- stellar.ops_by_source, which exists because a served route left to a
-- skip-index scan measured 110x slower than a PK-prefixed one. The
-- by_created twin is filled FROM the by-creator staging arm rather than
-- by re-aggregating the archive, so the second ordering costs a re-sort
-- of 21 M already-aggregated rows and not a second pass over 24.8 M.
--
-- WHY NOT READ stellar.account_creators_ops DIRECTLY. It is already
-- ORDER BY (creator, ...), so the outbound direction would in principle
-- work off it. It is a WORKING table: the cycle TRUNCATEs it and refills
-- it over 65 walked windows, so for the length of a cycle it holds a
-- PARTIAL archive. These two are staged and EXCHANGEd with the board, so
-- a served read never sees a half-built graph.
--
-- COST. The aggregation measured 3.14 GiB / 24.5 s on r1 2026-09-09 at
-- max_threads=2 over the whole working table. That is BELOW the cycle's
-- existing peak — the board's account-population join, 3.31 GiB — so the
-- cycle's ceiling is unchanged by these steps. The headroom is a stated
-- function of one population: about 150 bytes per distinct pair, so the
-- 8 GiB budget is reached near 55 M pairs against today's 21 M.
CREATE TABLE IF NOT EXISTS stellar.account_creator_edges
(
    creator        String,
    created        String,
    -- creations counts the CreateAccount operations behind this ONE
    -- edge; it exceeds 1 exactly for the recycling case above.
    -- funded_stroops sums their starting balances and is legitimately 0
    -- for CAP-33 sponsored creations, which pay no reserve of their own.
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
