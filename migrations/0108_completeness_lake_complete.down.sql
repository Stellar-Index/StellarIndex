-- 0108 down — drop `lake_complete` and restore the 0052 table comment.
BEGIN;

COMMENT ON TABLE completeness_snapshots IS
    'Per-source completeness watermark (ADR-0033). watermark_ledger is '
    'the highest ledger where substrate+recognition+projection all hold '
    'from genesis; coverage_pct = verified/span with no sparsity '
    'threshold. Written by compute-completeness, read by the API.';

ALTER TABLE completeness_snapshots
    DROP COLUMN IF EXISTS lake_complete;

COMMIT;
