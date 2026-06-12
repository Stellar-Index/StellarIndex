-- 0052 up — `completeness_snapshots` (ADR-0033 Phase 6 headline).
--
-- One row per source carrying the completeness WATERMARK: the highest
-- ledger such that all three claims hold contiguously from the source's
-- genesis — substrate continuity + hash chain (Claim 1), recognition
-- (Claim 2a), projection reconciliation (Claim 2b). This REPLACES
-- density_pct / gap_free_pct as the confidence signal: there is no
-- sparsity threshold here, so a single proven problem pins the
-- watermark and coverage_pct is honest by construction.
--
-- Written by `stellarindex-ops compute-completeness` (operator/cron),
-- read by /v1/diagnostics/ingestion + the status page — the same
-- compute-once / read-cheap shape as source_coverage_snapshots (0048).
--
-- first_problem_ledger is the exact ledger where the earliest failing
-- claim sits (0 = none): where to look / backfill.
--
-- Retention: NONE (one row per source, overwritten each compute).

BEGIN;

CREATE TABLE completeness_snapshots (
    source                text             PRIMARY KEY,
    genesis_ledger        bigint           NOT NULL,
    tip_ledger            bigint           NOT NULL,
    watermark_ledger      bigint           NOT NULL,
    coverage_pct          double precision NOT NULL,
    complete              boolean          NOT NULL,
    first_problem_ledger  bigint           NOT NULL DEFAULT 0,

    -- Per-claim booleans for the status-page breakdown. A claim that
    -- was not evaluated for this source (e.g. recognition for SDEX,
    -- which has no Soroban events) is recorded true with a note in
    -- `detail` rather than failing the verdict.
    substrate_ok          boolean          NOT NULL,
    recognition_ok        boolean          NOT NULL,
    projection_ok         boolean          NOT NULL,

    detail                text             NOT NULL DEFAULT '',
    computed_at           timestamptz      NOT NULL DEFAULT now()
);

COMMENT ON TABLE completeness_snapshots IS
    'Per-source completeness watermark (ADR-0033). watermark_ledger is '
    'the highest ledger where substrate+recognition+projection all hold '
    'from genesis; coverage_pct = verified/span with no sparsity '
    'threshold. Written by compute-completeness, read by the API.';

COMMIT;
