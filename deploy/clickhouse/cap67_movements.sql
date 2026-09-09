-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically — the gate named below pins that,
-- so the DDL here is a mirror and the runbook is the reason the file exists.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- cap67_movements_watermark — resume + serving watermark for the
-- `stellarindex-ops ch-cap67-movements` derive (inventory #1,
-- docs/operations/open-fixes-inventory-2026-08-08.md): the post-P23
-- continuation of stellar.account_movements, derived from the lake's
-- CAP-67 transfer events for EVERY asset (native XLM included —
-- deliberately unwatched by the Postgres sep41_transfers projection).
--
-- One row, replaced on every completed window. The API's movements
-- handler floors its Postgres tail arm at this watermark, so the two
-- arms stay gap-free and double-count-free at any derive progress.
-- The derived rows themselves land in stellar.account_movements with
-- provenance = 'cap67_derived' (RMT — re-derives collapse).
CREATE TABLE IF NOT EXISTS stellar.cap67_movements_watermark
(
    name        String,
    thru_ledger UInt32,
    updated_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name;
