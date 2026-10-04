-- 0206 down — no-op. The deleted checkpoints only let a projected-rebuild
-- skip windows whose rows 0137 and 0164 deleted or 0203 never wrote; restoring
-- them would bring the skip back.
SELECT 1;
