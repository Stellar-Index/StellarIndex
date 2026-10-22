-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the runbook it carries. A FRESH host
-- gets these objects from deploy/clickhouse/tier1_schema.sql, which declares
-- each of them identically — scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- Entry index: WHICH ledgers an account or an asset has classic-entry
-- activity in, and nothing else. `stellarindex-ops ch-entry-index` walks
-- stellar.ledger_entry_changes once per window and writes:
--
--   account_entry_ledgers — (account, role, ledger). owner rows come from
--     the lake's account_id column (account/trustline/offer/data entries);
--     claimant rows from decoding claimable-balance entries; sponsor rows
--     from decoding entries whose XDR tail carries a sponsor; signer rows
--     from set_options operations that add or reweight an ed25519 signer. account is the
--     raw 32-byte ed25519 key, not the G-strkey, so the key is fixed-width.
--   asset_entry_ledgers — (asset, entry_type, ledger) with that ledger's
--     created/updated/removed trustline change counts for the asset.
--
-- The index carries no entry payload: a reader resolves the ledgers here and
-- then reads the lake rows for exactly those ledgers, which keeps the index
-- to a few bytes per (key, ledger) instead of a second copy of every entry.
-- ReplacingMergeTree with no version column: a re-derived window collapses
-- into the same rows on merge. Progress lives in
-- stellar.entry_history_watermark under name 'entry_index'.
--
-- Runbook:
--   1. Measure first (no writes; prints row counts per window):
--        stellarindex-ops ch-entry-index -ch-addr 127.0.0.1:9300 -from <l> -to <l>
--   2. Apply this file once:
--        clickhouse-client --multiquery < deploy/clickhouse/entry_index.sql
--   3. Build from the first ledger whose rows carry account_id (the ec-floor);
--      the watermark's coverage proof refuses windows below it:
--        stellarindex-ops ch-entry-index -ch-addr 127.0.0.1:9300 -write -floor-ledger <ec-floor>
--   4. Set EXTRA_FLAGS=-floor-ledger <ec-floor> in /etc/default/entry-index, then
--        systemctl enable --now entry-index.timer
--      ansible installs the units but never enables the timer: enabled before
--      steps 2-4 it would fail every 10 minutes and re-scan its first window.
--   5. Re-derive one window below the watermark after a lake repair:
--        stellarindex-ops ch-entry-index -ch-addr 127.0.0.1:9300 -from <l> -to <l> -write
--
-- Sponsor rows cover entry sponsorship only (the sponsor in the entry's
-- extension). An account that sponsors another account's SIGNERS appears in
-- that account's extension, not the entry tail, and gets no sponsor row.

CREATE TABLE IF NOT EXISTS stellar.account_entry_ledgers
(
    account FixedString(32),
    role    Enum8('owner' = 1, 'claimant' = 2, 'sponsor' = 3, 'signer' = 4),
    ledger  UInt32
)
ENGINE = ReplacingMergeTree
PARTITION BY intDiv(ledger, 10000000)
ORDER BY (account, role, ledger);

-- IF NOT EXISTS keeps an older table's enum; the signer role (set_options) is appended in place, a metadata-only
-- change ClickHouse allows on a sort-key column.
ALTER TABLE stellar.account_entry_ledgers
    MODIFY COLUMN role Enum8('owner' = 1, 'claimant' = 2, 'sponsor' = 3, 'signer' = 4);

CREATE TABLE IF NOT EXISTS stellar.asset_entry_ledgers
(
    asset      String,
    entry_type LowCardinality(String),
    ledger     UInt32,
    n_created  UInt32,
    n_updated  UInt32,
    n_removed  UInt32
)
ENGINE = ReplacingMergeTree
PARTITION BY intDiv(ledger, 10000000)
ORDER BY (asset, entry_type, ledger);

-- account_claimables: one row per (claimant, claimable balance) from the same decode pass, carrying the newest entry
-- version seen, so the claimable-balances route reads a key range instead of scanning every change in the claimant
-- ledgers. claimable_removals: the ledger each claimable balance was claimed or clawed back; removal rows name no
-- claimant, so liveness is a key lookup here rather than in the 5B-row ledger_entries_current.
CREATE TABLE IF NOT EXISTS stellar.account_claimables
(
    account   FixedString(32),
    key_xdr   String,
    ledger    UInt32,
    entry_xdr String CODEC(ZSTD(3))
)
ENGINE = ReplacingMergeTree(ledger)
ORDER BY (account, key_xdr);

CREATE TABLE IF NOT EXISTS stellar.claimable_removals
(
    key_xdr String,
    ledger  UInt32
)
ENGINE = ReplacingMergeTree(ledger)
ORDER BY key_xdr;
