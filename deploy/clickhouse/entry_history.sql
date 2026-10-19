-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host gets entry_history_watermark from
-- deploy/clickhouse/tier1_schema.sql, which declares it identically —
-- scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- entry_history_watermark is shared by the entry index (`entry_index`), the
-- signer index and the sponsorship derive (`sponsorship_events`), one row per
-- advance under each name.
--
-- Retired: account_entry_changes and asset_entry_changes held the decoded
-- entry-change payloads. Served per-account and per-asset entry history now
-- decodes on read over the entry index (entry_index.sql), so nothing reads or
-- writes them. On a host that still has them, confirm both are empty, then:
--
--   SELECT count() FROM stellar.account_entry_changes;
--   SELECT count() FROM stellar.asset_entry_changes;
--   DROP TABLE IF EXISTS stellar.account_entry_changes;
--   DROP TABLE IF EXISTS stellar.asset_entry_changes;

CREATE TABLE IF NOT EXISTS stellar.entry_history_watermark
(
    name        String,
    thru_ledger UInt32,
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name;
