-- 0160 down — drop the cached independent-listing directory.
--
-- Loses nothing that is not re-derivable: every row is a copy of a third
-- party's currently published catalogue, and one `stellarindex-ops
-- listing-sync -write` pass rebuilds the whole table. There is no local
-- state here, no history, and no column any other table references.
DROP TABLE IF EXISTS asset_listing_directory;
