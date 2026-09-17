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
--   1. Apply the DDL (idempotent; every statement is IF NOT EXISTS):
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
-- account_cohort_* — what the accounts this address CREATED or SPONSORED
-- went on to hold and do. The sponsor and creator boards (#351) rank an
-- address by how many accounts it stood behind; these tables answer the
-- question that ranking invites — what value did that cohort bring to
-- the network — from the cohort's own ledger footprint: current
-- holdings, monthly flows in and out, the contracts it moved value
-- through, how recently it was active, and its open DeFi positions.
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

-- Open DeFi positions held by the cohort, per protocol / venue / asset.
-- `amount` is a Float64 sum of the folds' decimal amounts — a magnitude
-- for ranking and display, not a settlement figure.
CREATE TABLE IF NOT EXISTS stellar.account_cohort_positions
(
    rel           LowCardinality(String),
    root          String,
    protocol      LowCardinality(String),
    position_kind LowCardinality(String),
    venue         String,
    asset         String,
    holders       UInt64,
    amount        Float64
)
ENGINE = MergeTree
ORDER BY (rel, root, protocol, venue, asset, position_kind);

CREATE TABLE IF NOT EXISTS stellar.account_cohort_positions_staging
AS stellar.account_cohort_positions;

