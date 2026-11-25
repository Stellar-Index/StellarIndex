-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1); a FRESH host's tier1_schema.sql already declares these
-- codecs inside its CREATE TABLE.
--
-- ZSTD(3) on the high-entropy ledger_entry_changes columns (tx_hash, account_id,
-- asset, balance) and Delta+ZSTD(3) on intra_ledger_seq. Metadata-only and
-- idempotent: only NEW parts are written with the codecs. Existing parts keep LZ4
-- until `ALTER TABLE stellar.ledger_entry_changes REWRITE PARTS IN PARTITION <n>`
-- (a mutation: rewrites every column of the partition's parts, keeps duplicates).
-- Do not run it with SYSTEM STOP MERGES in force: that also stalls mutations.
ALTER TABLE stellar.ledger_entry_changes
    MODIFY COLUMN account_id CODEC(ZSTD(3)),
    MODIFY COLUMN tx_hash CODEC(ZSTD(3)),
    MODIFY COLUMN asset CODEC(ZSTD(3)),
    MODIFY COLUMN balance CODEC(ZSTD(3)),
    MODIFY COLUMN intra_ledger_seq CODEC(Delta, ZSTD(3));
