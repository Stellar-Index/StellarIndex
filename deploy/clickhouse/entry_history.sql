-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the runbook it carries. A FRESH host
-- gets these objects from deploy/clickhouse/tier1_schema.sql, which declares
-- each of them identically — scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- Entry-change history payload tables. The decode pass that filled them is
-- retired: `stellarindex-ops ch-entry-history` now derives only
-- stellar.sponsorship_events (runbook in sponsorship_events.sql), and the served
-- per-account and per-asset entry history decodes on read over the entry index
-- (entry_index.sql). The tables stay declared, with whatever rows a host already
-- holds, until their own operator DROP.
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
-- Both: `changed` lists the decoded fields an update altered (every field when
-- no pre-image was captured); `fields` is the decoded entry as JSON (the
-- pre-image for a removal); amounts in it are integer stroop strings, balance
-- is Int128 stroops. entry_history_watermark is shared with the entry index
-- (`entry_index`) and the sponsorship derive (`sponsorship_events`), one row
-- per advance under each name.

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
