-- 0136 up — account_directory: curated third-party labels for
-- well-known Stellar addresses (both G-account and C-contract
-- strkeys — the upstream set tags contract addresses like the
-- Aquarius AMM pools too).
--
-- Source of record: github.com/stellar-expert/public-directory
-- (MIT-licensed, explicitly published for integration — "All data
-- from this repository is publicly available for developers and
-- users free of charge"). Synced by `stellarindex-ops
-- directory-sync`, which upserts the full set and prunes rows the
-- upstream removed, scoped by `source` so a future second directory
-- source can coexist without the syncs deleting each other's rows.
--
-- These are DISPLAY labels with third-party attribution, not a
-- trust surface: nothing here feeds verification, the verified
-- currency catalogue, or scam suppression (S-010 stays curated
-- in-repo). The `malicious` / `unsafe` tags are surfaced as
-- warnings with their source named.
CREATE TABLE IF NOT EXISTS account_directory (
    address    text PRIMARY KEY CHECK (address ~ '^[GC][A-Z2-7]{55}$'),
    name       text NOT NULL,
    domain     text NOT NULL DEFAULT '',
    tags       text[] NOT NULL DEFAULT '{}',
    source     text NOT NULL DEFAULT 'stellar-expert',
    synced_at  timestamptz NOT NULL DEFAULT now()
);

-- Tag-scoped listings (e.g. "all #malicious entries") — GIN over the
-- array. The table is ~18.5k rows today; the index is cheap and keeps
-- tag filters off a seq scan as the set grows.
CREATE INDEX IF NOT EXISTS account_directory_tags_idx
    ON account_directory USING gin (tags);

-- Prune scans are `WHERE source = $1 AND synced_at < $2`.
CREATE INDEX IF NOT EXISTS account_directory_source_synced_idx
    ON account_directory (source, synced_at);
