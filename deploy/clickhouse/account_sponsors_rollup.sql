-- account_sponsors rollup — the "who has sponsored other accounts"
-- league table behind GET /v1/accounts/sponsors (#351).
--
-- SOURCE, AND WHY IT IS THE OPERATION STREAM AND NOT THE ENTRIES.
-- Sponsorship is both a state and a set of operations. The STATE — who
-- currently pays an entry's base reserve — lives in
-- LedgerEntry.ext.v1.sponsoringID, inside the base64 entry_xdr blob on
-- stellar.ledger_entries_current, projected as no column anywhere. The
-- OPERATIONS are already decoded in stellar.operations with op_type as
-- a filterable LowCardinality column. This rollup reads the operations,
-- and therefore answers the history question, not the state question.
-- What it cannot answer is written into the API description and the
-- page rather than papered over.
--
-- ONLY THE OPERATIONS THAT APPLIED. stellar.operations has no success
-- gate: the lake stores what the ledger CONTAINED, so it retains the
-- operations of transactions that FAILED, by design. A sponsorship
-- arrangement inside a failed transaction never took effect, so the
-- fill joins each operation to its transaction on the full (ledger_seq,
-- tx_index) identity and keeps it only where that transaction succeeded.
-- Measured on r1 2026-09-07, 2,426,813 of the archive's 22,413,991
-- sponsorship operations (10.8%) sit in failed transactions, rising to
-- 61.6% of partition 39; ungated the board served 11,162,397
-- sponsorships_started and 87,193 revocations_issued against true
-- figures of 9,972,887 and 39,492, ranked 323 accounts whose every
-- credited operation had failed, and placed 2,408 of the other 2,417 at
-- the wrong rank (#494).
--
-- WHY THE TRANSACTION AND NOT AN APPLIED EFFECT. The sibling creator
-- board gates by pairing its operation with a CAP-67 transfer movement,
-- which a failed transaction cannot produce, so the pairing IS the
-- gate. Sponsorship has no such effect to pair with: under CAP-33 the
-- is-sponsoring-future-reserves-for relationship lives only for the
-- duration of the transaction and is written to no ledger entry.
-- Measured on r1 2026-09-07 over ledgers 63,000,000-63,010,000, 0 of
-- 4,745 Begin and End operations have any stellar.ledger_entry_changes
-- row at their own (ledger_seq, tx_hash, op_index) — the 4,294 that DID
-- apply included. Only Revoke leaves an entry change (2 of 2), and
-- finding which entry needs the body_xdr decode this rollup exists to
-- avoid. The transaction's own successful flag is the only evidence of
-- application available for the two operation types that produce
-- sponsorships_started.
--
-- THE DERIVATION CARRIES NO XDR DECODE. Within a transaction a
-- BeginSponsoringFutureReserves is sourced by the SPONSOR and names the
-- sponsored account in its body; the matching
-- EndSponsoringFutureReserves is sourced by the SPONSORED ACCOUNT
-- itself and has a void body. So the sponsored identity is readable
-- straight off the End operation's source_account, with no base64 or
-- XDR work at all.
--
-- Verified on r1 2026-09-05, two independent ways:
--   * 6/6 sampled Begin bodies unmarshalled with the Stellar SDK's XDR
--     decoder yield a SponsoredId that equals the End operation's
--     source_account in the same transaction.
--   * Over ledgers 64,000,000-64,277,243 the body-decoding aggregation
--     and this body-free one return identical boards — same sponsors,
--     same sponsorships_started, same distinct_sponsored, same
--     revocations_issued.
-- Avoiding body_xdr is what makes the job affordable: reading it costs
-- 61.06 GiB over that window against 12.05 GiB without, because
-- body_xdr is the table's wide column and is read a whole granule at a
-- time.
--
-- ATTRIBUTION RULE. All End operations in a transaction are attributed
-- to that transaction's single Begin source. Sandwiches nest, so in
-- principle the pairing is a stack over op_index — but the rule above
-- is exact whenever a transaction has exactly ONE distinct Begin
-- source, which was true of 141,356 of 141,356 transactions measured.
-- Transactions with more than one distinct sponsor are EXCLUDED from
-- attribution and counted in account_sponsors_stats as ambiguous_txs,
-- so the case is visible in the served response rather than silently
-- mis-attributed.
--
-- COVERAGE IS DATA-DERIVED (ADR-0031) AND ITS FLOOR IS A FACT ABOUT THE
-- CHAIN. The cycle records the ledger span it actually aggregated. That
-- floor lands at protocol 14's activation, where sponsorship was
-- introduced: measured on r1, ledgers 32,600,000-32,747,294 contain
-- ZERO BeginSponsoringFutureReserves operations and they begin at
-- 32,747,295. The API and page present that as the feature's own
-- genesis, not as a gap in what was indexed.

-- Narrow, deduplicated projection of every APPLIED sponsorship
-- operation. Not served; it exists so the big table is read ONCE per
-- cycle and every served figure derives from the same materialized rows.
-- stellar.operations is a ReplacingMergeTree, so duplicates are
-- collapsed over its full ORDER BY key rather than trusted away — and so
-- is stellar.transactions, whose duplicates are present in bulk: over
-- ledgers 63,000,000-63,099,999 every one of that range's 33,380,486
-- distinct (ledger_seq, tx_index) keys carries more than one row. The
-- fill therefore groups by the operation identity, which absorbs the
-- multiplication a raw join would introduce, and resolves the success
-- flag with argMax over ingested_at rather than reading it off whichever
-- duplicate the scan reached first.
--
-- THE PASS THAT FILLS IT IS WALKED, one 1M-ledger lake partition per
-- statement. Unwalked it is a single indivisible statement over a
-- 24.74-billion-row, 2.18 TiB archive joined to a second one larger
-- still, and everything it holds grows with the chain: its wall time,
-- its dedupe state (one argMax per sponsorship operation in all of
-- history), and the join's build side. Measured on r1 2026-09-07 at
-- max_threads=2, one gated partition costs 18.9 s / 234.85 MiB
-- (partition 40, 164,670 applied operations), 31.5 s / 1.00 GiB
-- (partition 62, 1,013,943), 40.7 s / 1.28 GiB (partition 63, 1,033,738)
-- or 20.1 s / 1.68 GiB (partition 55, 2,410,901). Partition 55 is the
-- widest by MEMORY and partition 63 the slowest — they are not the same
-- window, and 1.68 GiB is the figure a future ceiling is sized from.
-- Ungated the same partitions cost 143.79 MiB, 705.21 MiB, 769.67 MiB
-- and 1.01 GiB, so the gate adds 67% at the widest. All of it sits
-- inside a unit whose TimeoutStartSec is 150 min. Walking
-- makes each statement's cost a function of one partition, and
-- per-window progress visible in the journal rather than only success or
-- failure at the end. Grouping per window is exact: both tables are
-- PARTITION BY intDiv(ledger_seq, 1000000) with ledger_seq leading the
-- ORDER BY, so no duplicate group straddles a window and no transaction
-- lands in a different window from its own operations. Each side carries
-- its OWN window predicate, because a join condition prunes neither.
--
-- The build side is pinned to the operations, which is the small side by
-- 128.5 to 1 in partition 55 (2,475,607 sponsorship operations before the
-- gate drops the failed ones, against 318,127,649 transactions) and 460 to
-- 1 in partition 63 (1,129,122 against 519,663,457). Below sponsorship's own
-- genesis there is no build side at all and the arm costs 0.01-0.29 s at
-- under 6 MiB, so the 32 windows under ledger 32,747,295 add nothing
-- measurable.
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
    computed_at          DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY rank;

CREATE TABLE IF NOT EXISTS stellar.account_sponsors_rollup_staging
AS stellar.account_sponsors_rollup;

-- Metric-keyed, like the sibling rollups. Carries the exact totals, the
-- data-derived coverage span, and ambiguous_txs — the attribution
-- escape hatch, published rather than hidden.
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
