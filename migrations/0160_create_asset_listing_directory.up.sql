-- 0160 up — `asset_listing_directory`: an independent third-party
-- price-aggregation platform's own per-coin platform→address map,
-- cached locally and read from cache.
--
-- Source of record: CoinGecko's `/api/v3/coins/list?include_platform=true`,
-- the public catalogue every listed coin appears in, joined to
-- `/api/v3/coins/markets` for that coin's published USD price. Synced by
-- `stellarindex-ops listing-sync`, which upserts the full Stellar slice
-- and prunes rows the upstream no longer carries — the `account_directory`
-- arrangement, migration 0136.
--
-- SINGLE SOURCE, DELIBERATELY. The prune is scoped by `source`, but the
-- primary key is the ADDRESS alone, so the two do not add up to
-- multi-source coexistence and this table must not be described as
-- supporting it. Two platforms listing the same address would fight over
-- one row: the later upsert flips `source` and replaces the price, and
-- the loser's next prune (same source, older synced_at) then deletes the
-- row the winner is publishing. Admitting a second platform means making
-- the key `(source, address)` FIRST, in a new migration, and teaching the
-- reader that an address can now carry more than one row.
--
-- WHY IT IS CACHED AND NOT FETCHED. The upstream catalogue is ~21k coin
-- objects / ~3.7 MB for the ~50 that carry a Stellar address, behind a
-- metered API key. Reading it on a request path would put a third party's
-- availability, rate limit and latency inside this index's own; syncing it
-- on a timer puts a bounded, inspectable table there instead. The two
-- staleness bounds below are what make the cache honest.
--
-- WHAT THIS TABLE IS. A convenience map, built for price aggregation:
-- "the platform that publishes a price for coin X says X lives at this
-- Stellar address". It CORROBORATES an address an independent party
-- already believed in — one more party having independently arrived at
-- the same (address → instrument) pairing — and that is the whole of its
-- authority.
--
-- WHAT THIS TABLE IS NOT. It is NOT an identity attestation system. The
-- upstream carries no signature, no proof of control of the address, and
-- no undertaking that the mapping is correct; a listing is a commercial
-- and editorial decision made by a third party, not a cryptographic
-- claim. It carries NO scam flags and admits none — there is no column
-- here for a negative verdict and there must not be one, because an
-- absence from this table means only "not listed", which is the normal
-- condition of almost every asset on the network and can never be read as
-- an accusation. Nothing here may be used to withhold, demote or suppress
-- an asset; the in-repo curated scam list (S-010) and the curated
-- directory's own tag vocabulary remain the only surfaces that do that.
--
-- THE TWO STALENESS BOUNDS, and why there are two. This table holds two
-- facts from two different clocks, and they rot at completely different
-- rates:
--
--   `synced_at`  — OUR clock: when this sync pass wrote the row. It
--                  bounds RECOGNITION, i.e. how long the row may still be
--                  read as "the platform lists this address". Enforced in
--                  the reader's SQL as listingRecognitionMaxAge (48h). An
--                  address→instrument mapping does not rot by the hour,
--                  and a bound tight enough to flap would make the
--                  admitted set flicker on a single missed pass.
--
--   `priced_at`  — the PLATFORM's clock: the `last_updated` the platform
--                  itself published alongside the price. It bounds the
--                  PRICE, and only the price, as listingPriceMaxAge (24h).
--                  It is deliberately NOT `synced_at`: a sync that
--                  succeeded while the upstream's own price was frozen
--                  would otherwise launder a stale price as fresh, which
--                  is exactly the failure the divergence layer's
--                  `staleness()` gate exists to refuse. A row whose
--                  `priced_at` is NULL is therefore UNPRICED, never
--                  "priced, timestamp unknown" — a missing publication
--                  time makes freshness unverifiable, and unverifiable is
--                  rejected, not waved through.
--
-- Past the recognition bound a row stops being served and the admitted
-- set SHRINKS — fail-closed, and counted (ListingDirectoryCensus.Stale)
-- so the shrink is visible rather than silent. Past the price bound the
-- row is still recognised and simply carries no price.
--
-- `price_usd` is NUMERIC per ADR-0003 and is carried as a decimal string
-- end to end — the upstream ships a JSON number, which is an IEEE-754
-- double the moment anything parses it as one, so the sync keeps the
-- literal and never round-trips through float64.
--
-- Fresh-database note: the table is created EMPTY and nothing needs
-- re-materializing. It fills on the first `listing-sync` pass (hourly
-- timer); until then every reader sees an empty recognised set, which is
-- the same state as an upstream that lists nothing.

BEGIN;

CREATE TABLE IF NOT EXISTS asset_listing_directory (
    -- The Stellar address the platform published for this coin, in
    -- EITHER of the two forms the network has: a Soroban contract
    -- C-strkey, or a classic `CODE-GISSUER` pair.
    --
    -- The CHECK admits exactly those two and nothing else — same posture
    -- as 0136's `^[GC][A-Z2-7]{55}$`, widened because a classic asset id
    -- is not a strkey. Asset codes are case-SENSITIVE alphanumerics of
    -- 1-12 characters, so the classic arm is `[A-Za-z0-9]`, not
    -- `[A-Z0-9]`: four live upstream rows (MXNe, sUSD, yUSDC, yETH) carry
    -- a lowercase letter in the code and an uppercase-only pattern would
    -- reject them as malformed. It is the same expression
    -- asset_volume_character_rollup.go already uses for a classic id.
    --
    -- PRIMARY KEY on the address, not on the upstream's coin id: the
    -- address is what every reader joins on, and two coin ids resolving
    -- to one address is an upstream contradiction that must collapse to
    -- one row rather than serve both.
    address    text        PRIMARY KEY
               CHECK (address ~ '^C[A-Z2-7]{55}$'
                   OR address ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'),

    -- The platform's own opaque coin slug ("stellar", "usd-coin"). Kept
    -- so a row can be traced back to the exact upstream record it came
    -- from, and so the price re-fetch can key on it.
    listing_id text        NOT NULL,

    -- The platform's display ticker, lowercase as published. Display
    -- only: it is NOT an asset code, it is NOT unique, and nothing may
    -- resolve an asset by it.
    symbol     text        NOT NULL DEFAULT '',

    -- The platform's published USD price. NUMERIC per ADR-0003 — the
    -- decimal the upstream printed, unrounded and never widened through
    -- a float. NULL means the platform published none, which is a normal
    -- condition, not an error.
    price_usd  numeric,

    -- The PLATFORM's own `last_updated` for that price — its clock, not
    -- ours. NULL whenever `price_usd` is NULL, and also whenever the
    -- upstream omitted or malformed the timestamp: a price whose
    -- freshness cannot be verified is not stored as a price.
    priced_at  timestamptz,

    -- Which aggregation platform this row came from. The prune is scoped
    -- by it; the upsert is NOT — it conflicts on the address alone. See
    -- the header for why that makes this a single-source table until the
    -- key changes.
    source     text        NOT NULL,

    -- OUR sync time. Written as now(), which is transaction-stable in
    -- Postgres — the whole sync lands with one identical value and the
    -- prune is then simply "same source, older synced_at". The reader's
    -- recognition bound is measured from this column.
    synced_at  timestamptz NOT NULL DEFAULT now(),

    -- A price and its publication time travel together or not at all.
    -- This is the NULL-priced_at rejection expressed in the table rather
    -- than trusted from the sync: a row reaching here by any other path
    -- still cannot claim a price with no verifiable age.
    CONSTRAINT asset_listing_directory_price_has_time CHECK (
        (price_usd IS NULL AND priced_at IS NULL)
     OR (price_usd IS NOT NULL AND priced_at IS NOT NULL)
    )
);

COMMENT ON TABLE asset_listing_directory IS
    'Cached per-coin platform-to-address map published by an independent '
    'price-aggregation platform, for the Stellar slice only, plus that '
    'platform''s own USD price. A convenience map that CORROBORATES an '
    'address, NOT an identity attestation system: no signatures, no proof '
    'of control, and no scam flags - absence means only "not listed". '
    'Two separate staleness bounds are enforced in the reader''s SQL: '
    'recognition on synced_at (our clock) and price on priced_at (the '
    'platform''s). Synced by `stellarindex-ops listing-sync`.';
COMMENT ON COLUMN asset_listing_directory.address IS
    'Stellar address as the platform published it, in either form: a '
    'Soroban contract C-strkey, or a classic CODE-GISSUER pair whose code '
    'is a CASE-SENSITIVE 1-12 char alphanumeric (MXNe, sUSD, yUSDC and '
    'yETH are live lowercase-bearing examples). Primary key because the '
    'address is what readers join on.';
COMMENT ON COLUMN asset_listing_directory.listing_id IS
    'The platform''s opaque coin slug, e.g. "stellar". Provenance and the '
    're-fetch key; never an identity this index resolves by.';
COMMENT ON COLUMN asset_listing_directory.symbol IS
    'The platform''s display ticker, lowercase as published. Not an asset '
    'code, not unique, and never a resolution key.';
COMMENT ON COLUMN asset_listing_directory.price_usd IS
    'The platform''s published USD price, exactly as printed. NUMERIC per '
    'ADR-0003 and carried as a decimal string end to end - the upstream '
    'ships a JSON number and parsing it as a double would lose digits. '
    'NULL when the platform published no price.';
COMMENT ON COLUMN asset_listing_directory.priced_at IS
    'The PLATFORM''s own last_updated for that price - its clock, not '
    'ours, and the column the 24h price bound is measured against. Bounding '
    'on synced_at instead would let a successful sync of a FROZEN upstream '
    'price serve it as fresh. NULL whenever there is no price or no '
    'verifiable publication time.';
COMMENT ON COLUMN asset_listing_directory.source IS
    'The aggregation platform this row came from. The prune is scoped by '
    'it; the upsert conflicts on the address alone, so this table holds '
    'ONE source at a time. A second platform needs the key to become '
    '(source, address) first, or the two syncs delete each other''s rows.';
COMMENT ON COLUMN asset_listing_directory.synced_at IS
    'OUR sync time, written as transaction-stable now(). The prune deletes '
    'same-source rows older than it, and the 48h recognition bound is '
    'measured from it.';

-- The prune scans `WHERE source = $1 AND synced_at < now()`, and the
-- reader bounds `synced_at` on every read. Stated plainly: at ~50 rows
-- neither needs an index to be fast, and this one is not carrying a
-- measurement. It is here because it costs nothing, because the same
-- pair on account_directory (0136) is the shape an operator reading
-- either table expects to find, and because the row count is set by how
-- many assets a third party chooses to list, which is not a number this
-- repo controls.
CREATE INDEX IF NOT EXISTS asset_listing_directory_source_synced_idx
    ON asset_listing_directory (source, synced_at);

COMMIT;
