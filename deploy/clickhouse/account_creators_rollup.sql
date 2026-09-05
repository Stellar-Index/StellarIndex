-- account_creators rollup — the "who bootstrapped the most accounts"
-- league table behind GET /v1/accounts/creators (#351).
--
-- SOURCE. stellar.account_movements, the feed-shaped classic-movement
-- archive (ADR-0048 D2). A `create_account` operation lands there as two
-- rows — direction='sent' on the funder and direction='received' on the
-- created account — with `counterparty` carrying the other side and
-- `amount` the starting balance in stroops. The funder arm alone
-- (direction='sent') is the creation record: one row per successful
-- CreateAccount, already decoded. Nothing has to be read out of
-- stellar.operations.body_xdr, which is undecoded base64 XDR.
--
-- WHY A ROLLUP. The predicate is movement_kind, which is not in
-- account_movements' ORDER BY (address, ledger, tx_hash, op_index,
-- leg_index, direction), so the aggregation is scan-shaped over the
-- whole 10.3-billion-row archive — measured 2026-09-05 at 2.1s /
-- 3.10 GiB for one 1M-ledger partition with max_threads=2, so ~65
-- partitions is a low-minutes full pass. That is a cycle cost, not a
-- request cost. The same staging + atomic EXCHANGE shape as
-- asset_holders_rollup / accounts_stats: readers are keyed and tiny.
--
-- COVERAGE IS DATA-DERIVED (ADR-0031). The cycle records the ledger
-- span it actually aggregated — min/max ledger and close time over the
-- create_account rows it read — into account_creators_stats. Nothing
-- assumes genesis. The API serves that span verbatim so the page can
-- state what it covers instead of implying the whole chain.
--
-- SCOPE. This is the CREATOR relationship only: funder → created
-- account, immutable history. The SPONSOR relationship (who currently
-- pays an entry's base reserve) is a different question with a
-- different source — LedgerEntry.ext.v1.sponsoringID, which is inside
-- the base64 entry_xdr blob on stellar.ledger_entries_current and is
-- not projected as a column anywhere. It is deliberately absent here
-- rather than approximated; see #351.

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
