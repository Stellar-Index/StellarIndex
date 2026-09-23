-- 0170 down — drop the override reason and its CHECK. Override rows
-- keep their source and survive every sync as before; only the
-- recorded justification is lost.
ALTER TABLE account_directory
    DROP CONSTRAINT IF EXISTS account_directory_override_reason_chk;
ALTER TABLE account_directory
    DROP COLUMN IF EXISTS override_reason;
