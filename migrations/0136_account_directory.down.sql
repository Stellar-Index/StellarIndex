-- 0136 down — remove the account_directory labels table. Data is
-- fully re-derivable from the upstream repo via `stellarindex-ops
-- directory-sync`; dropping loses nothing that can't be resynced.
DROP TABLE IF EXISTS account_directory;
