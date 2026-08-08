-- 0137 down: irreversible by design — the deleted rows carried the
-- migration-0059 event_index=0 defect this migration exists to remove,
-- and the correct rows come from `stellarindex-ops projector-replay
-- -source comet` (see the up migration), not from restoring the
-- defective set. No-op.
SELECT 1;
