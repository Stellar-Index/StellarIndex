-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically. Scope markers are enforced by
-- scripts/ci/lint-ch-apply-scope.sh.
--
-- trades_by_account: Postgres `trades` re-keyed account-first so
-- GET /v1/accounts/{g}/trades can page below the trades compression horizon.
--
-- Step 1 (tables, instant) -> Step 2 (ch-rebuild windows, HEAVY) -> Step 3.

CREATE TABLE IF NOT EXISTS stellar.trades_by_account
(
    account      String,
    ts           DateTime64(3, 'UTC'),
    ledger       UInt32,
    tx_hash      String,
    op_index     UInt32,
    source       LowCardinality(String),
    role         LowCardinality(String),
    counterparty String DEFAULT '',
    base_asset   String,
    quote_asset  String,
    base_amount  Int128,
    quote_amount Int128,
    usd_volume   String DEFAULT '',
    routed_via   String DEFAULT '',
    ingested_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (account, ts, ledger, tx_hash, op_index, source);

CREATE TABLE IF NOT EXISTS stellar.trades_by_account_windows
(
    from_ledger UInt32,
    to_ledger   UInt32,
    from_ts     DateTime('UTC'),
    to_ts       DateTime('UTC'),
    rows        UInt64,
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (from_ledger, to_ledger);

-- ── Step 2: backfill through ch-rebuild ─────────────────────────────────────
-- ***Heavy job, r1 only.*** No ClickHouse trades table exists, so the rows
-- come from the same lake decode ch-rebuild uses for Postgres, with the same
-- storability gate and usd_volume waterfall. -trades-by-account writes ONLY
-- ClickHouse (Postgres is untouched) and needs the full trade set: no
-- -sources, -contracts, -sdex-gaps or -sdex-reconcile. Windows of <= 2M
-- ledgers (the ch-rebuild buffer ceiling); re-running a window is idempotent.
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers")
--   for LO in $(seq 2 1000000 $TIP); do
--     HI=$((LO + 999999)); [ $HI -gt $TIP ] && HI=$TIP
--     /usr/local/sbin/run-heavy-job.sh "trades-by-account-$LO" \
--       stellarindex-ops ch-rebuild -config "$SI_CONFIG" \
--         -from $LO -to $HI -sdex -trades-by-account -write
--   done
--
-- Each successful window inserts one trades_by_account_windows row. The API
-- serves archive rows only from the contiguous run of windows that reaches the
-- Postgres compression horizon, so keep a daily run over [last to_ledger + 1,
-- lake tip] going after the backfill, or the archive falls behind the horizon
-- and the endpoint drops back to its horizon floor (lower_bound=true).
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
--   SELECT from_ledger, to_ledger, rows FROM stellar.trades_by_account_windows FINAL
--   ORDER BY from_ledger
-- Expect contiguous windows from ledger 2 to the tip; lower_bound turns false
-- once the run reaches ledger 2.
--
-- OPERATOR NOTE: a Postgres trades re-derive (ch-rebuild -write without
-- -trades-by-account) does not touch this table. Re-run the same windows with
-- -trades-by-account afterwards; a re-derive that changes identity columns
-- needs ALTER TABLE stellar.trades_by_account DROP PARTITION <ledger/1e6>
-- first.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
--   DROP TABLE IF EXISTS stellar.trades_by_account SYNC;
--   DROP TABLE IF EXISTS stellar.trades_by_account_windows SYNC;
