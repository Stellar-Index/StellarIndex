-- 0177 down — drop the override operator and its CHECK. Override rows
-- keep their source and reason and survive every sync as before; only
-- the recorded operator is lost.
ALTER TABLE account_directory
    DROP CONSTRAINT IF EXISTS account_directory_override_by_chk;
ALTER TABLE account_directory
    DROP COLUMN IF EXISTS override_by;
