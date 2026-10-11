-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below. A FRESH host gets these objects from
-- deploy/clickhouse/tier1_schema.sql, which declares each of them
-- identically — scripts/ci/lint-ch-apply-scope.sh pins that, so the DDL
-- here is a mirror and the runbook is the reason the file exists.
--
-- ── RUNBOOK (r1) ─────────────────────────────────────────────────────────
--
--   1. Apply the DDL (idempotent: IF NOT EXISTS, and the closing retype is
--      a no-op once applied):
--        clickhouse-client --port 9300 --multiquery < deploy/clickhouse/account_cohort_rollup.sql
--   2. Run one cycle by hand and read its step log — the movements walk
--      is the long step (the whole account_movements archive, one 1M-
--      ledger window at a time, joined to the cohort membership):
--        systemctl start cohort-rollup.service && journalctl -fu cohort-rollup
--   3. Confirm the swap landed and the API reads it:
--        SELECT rel, count(), max(computed_at) FROM stellar.account_cohort_roots GROUP BY rel
--        curl -s https://api.stellarindex.io/v1/accounts/<G…>/graph/cohort?relation=created | jq .data.coverage
--   4. The timer (cohort-rollup.timer, daily) takes over from here. The
--      cycle needs Postgres as well as ClickHouse — the DeFi position
--      snapshot is read from the served tier — so the unit carries
--      -config like the sync units, not only -ch-addr like the boards.
--
-- Working tables (account_cohort_members, account_cohort_parts_staging)
-- are truncated and refilled every cycle and never read by the API.
--
-- account_cohort_* — what the accounts an address CREATED or SPONSORED went
-- on to hold and do: current holdings, monthly flows, contracts moved value
-- through, recency, and open DeFi positions.
--
-- One cycle (stellarindex-ops ch-cohort-rollup) rebuilds every table into its
-- _staging twin and swaps atomically, so a reader never sees a half-built
-- cohort. An account belongs to the cohort of every root it has an edge from,
-- so it can count under its creator AND each sponsor — the two relations are
-- never summed.
--
-- COVERED ROOTS. Every sponsor, and every creator with at least
-- account_cohort_min_created accounts (internal/storage/clickhouse/
-- account_cohort_rollup.go). Below that the per-account pages are the better
-- read; the API says so rather than serving an empty cohort as "holds nothing".
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

-- Working table: cohort membership keyed by member so every source joins on its account column.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_members
(
    rel    LowCardinality(String),
    root   String,
    member String
)
ENGINE = MergeTree
ORDER BY (member, rel, root);

-- `asset` is the canonical id ledger_entries_current carries: 'native',
-- 'CODE-ISSUER', or 'pool:<hex>' for a classic liquidity-pool share.
-- Balances are stroops.
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

-- Working table: a month spans ledger windows of the movements walk, so the
-- walk writes partial rows with a mergeable distinct-account state and the
-- cycle folds them into flows and contracts. `contract` is the C… counterparty
-- of a movement, '' otherwise.
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

-- Monthly value moved in and out of the cohort, in the asset's smallest unit
-- (stroops for classic; a Soroban token's own scale for a C… id).
-- `active_accounts` is uniqCombined — a close estimate, not exact.
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

-- Contracts the cohort moved value through. Value-moving subset of
-- "interacted with": a call that moved no balance is not counted.
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

-- Members seen in the last 30 / 90 / 365 days, from account_activity's last-seen watermark.
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

-- Snapshot of every open Soroban DeFi position the served tier's folds know
-- (the six behind /v1/accounts/{g}/positions), copied from Postgres each cycle
-- so the cohort join happens here. `amount` is the fold's own decimal string
-- in the fold's own unit; `asset` is '' when denominated in venue shares.
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
-- monthly VWAP over its USD-quoted markets (prices_1mo, every alias spelling
-- folded — timescale.Store.MonthlyUSDVWAPs), copied from Postgres each cycle.
-- Flows LEFT JOIN it on (asset, month); a month with no USD-quoted market has
-- no row and is served unpriced, never at zero. `vwap_usd` is the exact
-- decimal string. XLM is under 'native'.
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

-- A deployment created with `amount Float64` keeps it under IF NOT EXISTS,
-- and its staging twin copies it, so retype both halves of the EXCHANGE pair
-- (a no-op once applied). Apply before rolling a binary whose reader scans
-- Int256; the next cycle rewrites every row exactly.
ALTER TABLE stellar.account_cohort_positions
    MODIFY COLUMN amount Int256;
ALTER TABLE stellar.account_cohort_positions_staging
    MODIFY COLUMN amount Int256;
