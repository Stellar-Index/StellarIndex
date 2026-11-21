-- 0235 up — restore a database CHECK on the trades amount legs: both
-- legs >= 0 and not both zero, the rule canonical.Trade.Validate enforces
-- since 0191 dropped 0001's `> 0` CHECKs. Without it a writer that skips
-- Validate (a raw-SQL repair, a new bulk path) can store a negative leg,
-- which no priceable filter keeps out of the volume sums.
--
-- NOT VALID: the CHECK binds every new and updated row from now on, but
-- existing rows are not scanned, so this is catalog-only and compressed
-- chunks stay compressed. Validating history is a separate operator step
-- (see migrations/README.md row 0235): count violators first, then
-- `ALTER TABLE trades VALIDATE CONSTRAINT trades_amount_legs_check` in a
-- quiet window.
--
-- Lock: ACCESS EXCLUSIVE on trades and its chunks for a catalog write —
-- sub-second of work, independent of table size, but it queues behind a
-- long reader, a compression run or a CAGG refresh, and new trades queries
-- queue behind it. As 0191: the deploy's PGOPTIONS lock_timeout bounds the
-- wait; if it trips, the DDL rolls back, schema_migrations is dirty at 235,
-- recover with `stellarindex-migrate force 234` and re-run in a quiet window.
-- Re-run: one transaction, so a failed apply leaves nothing behind and a
-- retry starts clean; golang-migrate never re-applies a recorded version.
-- Old-binary-safe (rule 9): the previous binary's Validate already refuses
-- every row this CHECK refuses.

BEGIN;

ALTER TABLE trades ADD CONSTRAINT trades_amount_legs_check  -- migration-compat:ok canonical.Trade.Validate in every released binary refuses the same rows
    CHECK (base_amount >= 0 AND quote_amount >= 0 AND (base_amount > 0 OR quote_amount > 0))
    NOT VALID;

COMMIT;
