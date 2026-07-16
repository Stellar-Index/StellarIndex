-- 0108 up — two-axis completeness verdict: add `lake_complete`
-- (notes/DECISION-genesis-complete-verdict-2026-07-16.md, Option B).
--
-- completeness_snapshots.complete has always meant substrate ∧
-- recognition ∧ projection — but projection reconcile for
-- trade-emitting sources is scoped to a retention window (ADR-0034:
-- Postgres is the SERVED tier, not the archive), so `complete` for
-- those sources has never been a genesis-to-tip claim even when the
-- certified ClickHouse lake (ADR-0034) genuinely IS contiguous +
-- hash-chained + recognition-complete from genesis. That watermark
-- (substrate ∧ recognition, no projection gate) was already computed
-- by compute-completeness — it was just discarded before being
-- written.
--
-- `lake_complete` surfaces it as its own column: "the certified
-- ClickHouse archive is contiguous + hash-chained + recognition-
-- complete from genesis to tip for this source's domain" — the
-- ADR-0033 substrate+recognition watermark's Complete, independent of
-- the retention-scoped projection reconcile. `complete` is unchanged
-- (substrate ∧ recognition ∧ projection); `watermark_ledger` /
-- `coverage_pct` are unchanged too — they were always
-- substrate∧recognition-scoped (see the corrected table comment
-- below), which is exactly the lake axis.
--
-- Additive with a DEFAULT so the currently-deployed binary (whose
-- upsert doesn't list this column) keeps working unmodified —
-- old-binary-safe per repo convention.

BEGIN;

ALTER TABLE completeness_snapshots
    ADD COLUMN lake_complete boolean NOT NULL DEFAULT false;

-- Corrects migration 0052's table comment, which claimed
-- watermark_ledger is "where substrate+recognition+projection all
-- hold" — the code has only ever excluded projection from the
-- watermark (projection is tracked as its own `projection_ok` bool
-- and gates `complete` separately); this was a stale/inaccurate
-- comment, not a behavior change.
COMMENT ON TABLE completeness_snapshots IS
    'Per-source completeness verdict (ADR-0033/ADR-0034). '
    'watermark_ledger/coverage_pct are the LAKE (archive) axis: the '
    'highest ledger where substrate+recognition (NOT projection) hold '
    'contiguously from genesis. lake_complete is that axis''s headline '
    '(watermark_ledger >= tip_ledger) — genesis-complete for the '
    'certified ClickHouse archive, decoupled from the served tier. '
    'complete is the SERVED/combined axis: lake_complete additionally '
    'gated by projection_ok, which is retention-scoped by design (ADR-0034: '
    'Postgres is the served tier, not the archive) — see '
    'notes/DECISION-genesis-complete-verdict-2026-07-16.md. Written by '
    'compute-completeness, read by the API.';

COMMIT;
