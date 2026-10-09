-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the runbook it carries. A FRESH host
-- gets these objects from deploy/clickhouse/tier1_schema.sql, which declares
-- each of them identically — scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- Entry-change history: `stellarindex-ops ch-entry-history` decodes every
-- classic stellar.ledger_entry_changes row (account, trustline, offer, data,
-- claimable_balance, liquidity_pool) ONCE and writes two projections of it:
--
--   account_entry_changes — keyed by account. One row per account the change
--     concerns, by role: owner (account/trustline/offer/data), sponsor (the
--     entry's or a signer's sponsor, before or after the change), claimant
--     (claimable balance). asset/balance: the owner's native or trustline
--     balance, or a claimant's claimable amount, after the change. Every
--     claimant carries the whole amount: never sum balance across claimants.
--   asset_entry_changes — keyed by asset. holder (trustline), selling/buying
--     (offer), claimable (account = the balance's sponsor), reserve_a/
--     reserve_b and pool (liquidity pool, keyed "pool:<hex>"). Account
--     entries are not here: native per-holder history is the account side.
--
-- Both: 'state' pre-images are not rows; `changed` lists the decoded fields
-- an update altered (every field when no pre-image was captured), so
-- fee/sequence-only bumps are a read-time filter, not a lost row. `fields`
-- is the decoded entry as JSON (the pre-image for a removal); amounts in it
-- are integer stroop strings, balance is Int128 stroops. RMT: re-derives collapse.
--
-- Runbook:
--   1. Measure the source per entry type (rows, stored bytes, tx-level rows):
--        SELECT entry_type, change_type, count() AS rows,
--               countIf(op_index = -1) AS tx_level_rows,
--               sum(length(entry_xdr) + length(key_xdr)) AS raw_xdr_bytes
--        FROM stellar.ledger_entry_changes
--        WHERE entry_type IN ('account', 'trustline', 'offer', 'data',
--                             'claimable_balance', 'liquidity_pool')
--        GROUP BY entry_type, change_type ORDER BY rows DESC;
--   2. Measure the derive on a sample (writes nothing; works before step 3):
--        stellarindex-ops ch-entry-history -ch-addr 127.0.0.1:9300 \
--          -from N -to N+99999
--      It prints rows and fields bytes per entry type and the count of
--      fee/sequence-only account rows.
--   3. Apply this file, then backfill from the entry-change coverage floor,
--      not the lake's first ledger: -write refuses to advance the watermark
--      over any tx-bearing ledger without a transaction-scoped
--      ledger_entry_changes row (ErrEntryHistoryEntryChangeShortfall). Read
--      the floor from verify-contiguity's Check 2 (its auto -ec-floor line
--      prints the lowest such ledger), then (resumable; a re-run without
--      -from continues at the watermark):
--        stellarindex-ops ch-entry-history -ch-addr 127.0.0.1:9300 -write \
--          -floor-ledger <ec-floor>
--      A window that refuses stops the run with the watermark unmoved;
--      re-run once ch-backfill has filled that window's entry changes.
--   4. Only once the derive covers the lake's first ledger (step 3 ran with
--      an ec-floor equal to the lake's first ledger), record the marker that
--      lets /v1/assets/{asset_id}/entry-changes drop lower_bound:
--        INSERT INTO stellar.entry_history_watermark (name, thru_ledger)
--        SELECT 'entry_history_backfill', max(thru_ledger)
--        FROM stellar.entry_history_watermark WHERE name = 'entry_history';
--      Rollback (back to lower_bound):
--        DELETE FROM stellar.entry_history_watermark WHERE name = 'entry_history_backfill';
CREATE TABLE IF NOT EXISTS stellar.account_entry_changes
(
    account          String,
    ledger           UInt32,
    close_time       DateTime('UTC'),
    tx_hash          String,
    op_index         Int32,
    change_index     UInt32,
    role             LowCardinality(String),
    intra_ledger_seq UInt32,
    entry_type       LowCardinality(String),
    change_type      LowCardinality(String),
    changed          Array(LowCardinality(String)),
    asset            String DEFAULT '',
    balance          Int128,
    fields           String DEFAULT '{}' CODEC(ZSTD(3)),
    ingested_at      DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (account, ledger, tx_hash, op_index, change_index, role);

CREATE TABLE IF NOT EXISTS stellar.asset_entry_changes
(
    asset            String,
    ledger           UInt32,
    close_time       DateTime('UTC'),
    tx_hash          String,
    op_index         Int32,
    change_index     UInt32,
    role             LowCardinality(String),
    intra_ledger_seq UInt32,
    entry_type       LowCardinality(String),
    change_type      LowCardinality(String),
    changed          Array(LowCardinality(String)),
    account          String DEFAULT '',
    balance          Int128,
    fields           String DEFAULT '{}' CODEC(ZSTD(3)),
    ingested_at      DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (asset, ledger, tx_hash, op_index, change_index, role);

CREATE TABLE IF NOT EXISTS stellar.entry_history_watermark
(
    name        String,
    thru_ledger UInt32,
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name;
