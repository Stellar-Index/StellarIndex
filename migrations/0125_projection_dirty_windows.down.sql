-- 0124 down — drop the projector-replay dirty-window record.
--
-- Reversal note: dropping the table discards any PENDING re-reconcile
-- obligations. If a projector-replay rewound below a source's watermark
-- and no clean compute-completeness run has covered that range yet, the
-- next incremental run will carry the prior projection claim over the
-- rewritten range unverified (the pre-0124 behaviour). After rolling
-- back, run compute-completeness WITHOUT -from (full-scope) for any
-- source replayed since the last clean full verify.

BEGIN;

DROP TABLE IF EXISTS projection_dirty_windows;

COMMIT;
