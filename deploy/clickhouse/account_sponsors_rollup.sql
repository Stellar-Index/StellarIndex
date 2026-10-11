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
-- account_sponsors rollup — the "who has sponsored other accounts"
-- league table behind GET /v1/accounts/sponsors.
--
-- SOURCE: the operation stream, not ledger entries. The sponsorship STATE (who
-- pays an entry's base reserve) lives in LedgerEntry.ext.v1.sponsoringID
-- inside entry_xdr and is projected as no column; the operations are already
-- decoded in stellar.operations. So this rollup answers the history question,
-- not the state question.
--
-- ONLY THE OPERATIONS THAT APPLIED. stellar.operations keeps the operations of
-- FAILED transactions by design, and a sponsorship inside a failed transaction
-- never took effect. The fill joins each operation to its transaction on the
-- full (ledger_seq, tx_index) identity and keeps it only where that
-- transaction succeeded. Ungated, about 11% of sponsorship operations (62% in
-- one partition) are failed ones and the board's counts and ranks are wrong.
--
-- WHY THE TRANSACTION AND NOT AN APPLIED EFFECT. The sibling creator board
-- gates by pairing with a CAP-67 transfer, which a failed transaction cannot
-- produce. Sponsorship has no such effect: under CAP-33 the sponsoring
-- relationship lives only for the duration of the transaction and is written
-- to no ledger entry (0 of 4,745 sampled Begin/End operations have a
-- ledger_entry_changes row). Revoke does leave one, but finding which entry
-- needs the body_xdr decode this rollup avoids. The transaction's successful
-- flag is the only evidence of application.
--
-- NO XDR DECODE. Within a transaction a BeginSponsoringFutureReserves is
-- sourced by the SPONSOR and an EndSponsoringFutureReserves by the SPONSORED
-- ACCOUNT, with a void body, so the sponsored identity is the End operation's
-- source_account.
--
-- Verified on r1, two independent ways:
--   * 6/6 sampled Begin bodies unmarshalled with the Stellar SDK's XDR
--     decoder yield a SponsoredId that equals the End operation's
--     source_account in the same transaction.
--   * Over ledgers 64,000,000-64,277,243 the body-decoding aggregation
--     and this body-free one return identical boards — same sponsors,
--     same sponsorships_started, same distinct_sponsored, same
--     revocations_issued.
-- Avoiding body_xdr is what makes the job affordable: it is the wide column,
-- 61 GiB read against 12 GiB over that window.
--
-- ATTRIBUTION RULE. All End operations in a transaction are attributed to its
-- single Begin source. Sandwiches nest, so in principle the pairing is a stack
-- over op_index, but the rule is exact whenever a transaction has one distinct
-- Begin source (141,356 of 141,356 measured). Transactions with more than one
-- distinct sponsor are EXCLUDED and counted in account_sponsors_stats as
-- ambiguous_txs, so the case is visible in the served response.
--
-- COVERAGE IS DATA-DERIVED (ADR-0031). The cycle records the ledger span it
-- actually aggregated. The floor is protocol 14's activation, where
-- sponsorship was introduced (first Begin at ledger 32,747,295); the API and
-- page present it as the feature's genesis, not as an indexing gap.

-- Narrow, deduplicated projection of every APPLIED sponsorship operation. Not
-- served; the big table is read ONCE per cycle and every served figure derives
-- from these rows. stellar.operations and stellar.transactions are
-- ReplacingMergeTree and hold duplicates (transactions in bulk), so the fill
-- groups by the operation identity and resolves the success flag with argMax
-- over ingested_at rather than reading it off whichever duplicate was scanned
-- first.
--
-- THE FILL IS WALKED, one 1M-ledger lake partition per statement: unwalked it
-- is one indivisible statement whose wall time, dedupe state and join build
-- side all grow with the chain. Largest measured partition is 1.68 GiB at
-- max_threads=2 (partition 55, widest by memory; 63 is slowest); size any
-- ceiling from that. Grouping per window is exact: both tables are PARTITION
-- BY intDiv(ledger_seq, 1000000) with ledger_seq leading the ORDER BY, so no
-- duplicate group or transaction straddles a window. Each side carries its OWN
-- window predicate, because a join condition prunes neither.
--
-- The build side is pinned to the operations, the small side (128:1 in
-- partition 55, 460:1 in partition 63). Windows below ledger 32,747,295 have
-- no build side and cost nothing measurable.
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
-- Immutable history: arrangements STARTED and distinct accounts ever
-- sponsored. These only grow.
    sponsorships_started UInt64,
    distinct_sponsored   UInt64,
-- Revocations ISSUED (source of a RevokeSponsorship). History, so
-- sponsorships_started is not a count of sponsorships in force.
    revocations_issued   UInt64,
    first_ledger         UInt32,
    last_ledger          UInt32,
    first_seen_at        DateTime('UTC'),
    last_seen_at         DateTime('UTC'),
    computed_at          DateTime DEFAULT now(),
-- ORDER BY rank serves the top-N page; the per-sponsor read filters on
-- sponsor, which without this index reads every granule.
    INDEX idx_sponsors_rollup_sponsor sponsor TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
ORDER BY rank;

CREATE TABLE IF NOT EXISTS stellar.account_sponsors_rollup_staging
AS stellar.account_sponsors_rollup;

-- IF NOT EXISTS keeps an older board's definition, so add the index to both
-- halves of the EXCHANGE pair. No MATERIALIZE INDEX: every cycle TRUNCATEs
-- staging and rewrites the whole board.
ALTER TABLE stellar.account_sponsors_rollup
    ADD INDEX IF NOT EXISTS idx_sponsors_rollup_sponsor sponsor TYPE bloom_filter(0.01) GRANULARITY 1;
ALTER TABLE stellar.account_sponsors_rollup_staging
    ADD INDEX IF NOT EXISTS idx_sponsors_rollup_sponsor sponsor TYPE bloom_filter(0.01) GRANULARITY 1;

-- Metric-keyed, like the sibling rollups. Carries the exact totals, the data-
-- derived coverage span, and ambiguous_txs (published, not hidden).
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

-- ── Sponsorship GRAPH edges ──────────────────────────────────
-- The board answers "who has sponsored the most"; these two tables answer the
-- graph question in both directions: whom has an account sponsored, and who
-- sponsored THIS account.
--
-- HISTORY, AS THE BOARD IS. An edge means "this sponsor began at least one
-- sponsorship covering this account", never "is currently sponsoring it".
-- There is no per-edge revoked/current flag: RevokeSponsorship names its entry
-- inside body_xdr, which this cycle does not decode, so a revocation cannot be
-- attributed to an edge. Entries also stop being sponsored by deletion or
-- account merge, which emit no operation. A "currently sponsoring" edge would
-- overstate; the live set needs the sponsoringID inside each ledger entry,
-- which is not projected.
--
-- ONE ROW PER DISTINCT (sponsor, sponsored) PAIR. The pair count equals the
-- board's distinct_sponsored_total and the event count its sponsorships_total,
-- so these decompose the board rather than second-guess it.
--
-- INBOUND IS SMALL, OUTBOUND IS NOT (at most 8 distinct sponsors per account
-- in partition 63; the top sponsor covers 785,543). So the inbound side is
-- capped and the outbound side paged by keyset cursor.
--
-- TWO TABLES, ONE CONTENT, TWO SORT ORDERS, as the sibling creator edges: each
-- direction is a primary-key range read. The by_sponsored twin is filled FROM
-- the by-sponsor staging arm rather than re-deriving attribution.
--
-- COST: 4.01 GiB at max_threads=2, just below the cycle's peak (the board
-- join, 4.09 GiB), so the ceiling is unchanged.
CREATE TABLE IF NOT EXISTS stellar.account_sponsor_edges
(
    sponsor              String,
    sponsored            String,
-- Arrangements STARTED between this pair, not ones still in force (see above).
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
