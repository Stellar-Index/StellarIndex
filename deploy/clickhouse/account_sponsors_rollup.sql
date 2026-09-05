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

-- Narrow, deduplicated projection of every sponsorship operation. Not
-- served; it exists so the big table is read ONCE per cycle and every
-- served figure derives from the same materialized rows.
-- stellar.operations is a ReplacingMergeTree, so duplicates are
-- collapsed over its full ORDER BY key rather than trusted away.
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
