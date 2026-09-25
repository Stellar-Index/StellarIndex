-- 0181 down — nothing is reverted. The up raised cooldowns below 300 s
-- to 300 without recording the old values, and those values were the
-- per-tick amplifier it exists to remove; a longer cooldown is valid
-- under every schema version, so leaving it in place is safe.

BEGIN;

SELECT 1;

COMMIT;
