-- 0184 down — deliberate no-op. The up deleted accusations whose own
-- evidence disproves them; there is nothing to restore them from, and
-- restoring them would re-publish impossible sandwiches naming real
-- accounts. The rows can only come back if a detector re-emits them, which
-- the direction rule now prevents.

SELECT 1;
