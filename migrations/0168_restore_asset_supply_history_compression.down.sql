-- 0168 down — intentionally a NO-OP.
--
-- The up only restores the compression 0005 and 0030 declare for
-- asset_supply_history, and only where it was missing. Rolling that back
-- would disable compression on a database where the up did nothing, which
-- is not the schema version 0166 describes. Forward-only by design.
SELECT 1;
