-- 0173 down — drop `usd_volume_restamp_log`. REFUSES while it holds rows:
-- they are the only record of what a restamp overwrote, and dropping them
-- makes every logged run irreversible. Export or delete them explicitly
-- first if that is really what you want.
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM usd_volume_restamp_log) THEN
    RAISE EXCEPTION '0173_usd_volume_restamp_log.down.sql: usd_volume_restamp_log still holds before-images — down-migrating would make every logged restamp irreversible. Export or delete them explicitly first.';
  END IF;
END $$;

DROP TABLE IF EXISTS usd_volume_restamp_log;

COMMIT;
