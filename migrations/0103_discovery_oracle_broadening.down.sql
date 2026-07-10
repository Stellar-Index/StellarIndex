-- 0103 down — restore the four-value first_seen_event CHECK and
-- drop discovery_kind. Any row recorded by one of the two new
-- sniffers (discovery_kind <> 'sep41', or first_seen_event outside
-- the original four values) must be removed first or the narrower
-- constraint re-add fails — deliberate: down-migrating with
-- broadened-sniffer data present should be loud, not silent (same
-- stance as 0070/0092/0094's down).
BEGIN;

DELETE FROM discovered_assets WHERE discovery_kind <> 'sep41';

ALTER TABLE discovered_assets DROP CONSTRAINT discovered_assets_first_seen_event_check;
ALTER TABLE discovered_assets ADD CONSTRAINT discovered_assets_first_seen_event_check CHECK (first_seen_event IN (
    'transfer',
    'mint',
    'burn',
    'clawback'
));

ALTER TABLE discovered_assets DROP COLUMN discovery_kind;

COMMIT;
