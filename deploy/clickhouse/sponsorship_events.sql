-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the runbook it carries. A FRESH host
-- gets this object from deploy/clickhouse/tier1_schema.sql, which declares it
-- identically; scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- Sponsorship lifecycle: one row per transition of one arrangement, derived by
-- `stellarindex-ops ch-entry-history` from the paired pre/post images it already
-- walks, as a third output under the entry_history watermark.
--
--   started  a sponsor began paying reserves for the entry or signer
--   revoked  the entry lives on, unsponsored or under another sponsor
--   ended    the entry or signer is gone
--
-- kind: account (2 reserves), trustline (1), pool_share (2), offer (1),
-- data (1), signer (1), claimable_balance (one per claimant, `sponsored` empty).
--
-- Runbook (operator, on the host; CREATE IF NOT EXISTS is idempotent):
--   1. clickhouse-client --multiquery < deploy/clickhouse/sponsorship_events.sql
--   2. deploy the release that writes it; new windows fill from the watermark.
--   3. fill history with `stellarindex-ops ch-entry-history -from <l> -to <l> -write`
--      per window below the watermark (ReplacingMergeTree collapses re-derived
--      account and asset rows). Start no earlier than ledger 32747293, the first
--      protocol-14 ledger on pubnet: no entry before it can carry a sponsor.

CREATE TABLE IF NOT EXISTS stellar.sponsorship_events
(
    sponsor          String,
    sponsored        String DEFAULT '',
    kind             LowCardinality(String),
    key_id           String DEFAULT '',
    ledger           UInt32,
    close_time       DateTime('UTC'),
    tx_hash          String,
    op_index         Int32,
    change_index     UInt32,
    intra_ledger_seq UInt32,
    reserves         UInt8,
    event            LowCardinality(String),
    ingested_at      DateTime DEFAULT now(),
    INDEX sponsored_bf sponsored TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (sponsor, sponsored, kind, key_id, ledger, tx_hash, op_index, change_index);

-- Retrofit for a host whose table predates the skip index (no-op right after the CREATE above).
ALTER TABLE stellar.sponsorship_events ADD INDEX IF NOT EXISTS sponsored_bf sponsored TYPE bloom_filter GRANULARITY 4;
