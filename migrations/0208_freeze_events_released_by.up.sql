-- 0208 up — record WHO closed a freeze_events row.
--
-- Both release paths close the row with the same UPDATE (recovered_at,
-- recovered_at_ledger), so the row cannot say whether an operator lifted
-- the freeze with `stellarindex-ops freeze-unfreeze` or the recovery
-- worker closed it after an auto/lapsed release. The only durable trace
-- was a fuzzy join on audit_log near recovered_at.
--
--   released_by   'operator:<actor>' — freeze-unfreeze; the reason lives in
--                 audit_log (action = 'freeze.unfreeze').
--                 'system:recovery' — the recovery worker.
--                 NULL — still open, or closed before 0208 (unknown; not
--                 back-filled from audit_log on purpose).
--
-- The value domain is enforced in Go (FreezeEventSink.MarkRecovered), not
-- by a CHECK: no CHECK has been added to this compressed hypertable before.
-- The column carries an operator's OS user name, so no API read path
-- selects it.
--
-- Additive (migrations/README.md rule 9): one nullable column, no default,
-- no constraint. The previous binary's UPDATE does not name the column and
-- keeps working, leaving it NULL. `freeze_events` is a compressed
-- hypertable; ADD COLUMN of a nullable no-default column rewrites no chunk
-- (as 0119 and 0163). Executed in
-- test/integration/freeze_events_released_by_test.go.

BEGIN;

ALTER TABLE freeze_events
    ADD COLUMN IF NOT EXISTS released_by text;

COMMENT ON COLUMN freeze_events.released_by IS
    'Who closed the row: ''operator:<actor>'' (stellarindex-ops '
    'freeze-unfreeze; reason in audit_log action=freeze.unfreeze) or '
    '''system:recovery'' (recovery worker after an auto/lapsed release). '
    'NULL while open, or closed before 0208.';

COMMIT;
