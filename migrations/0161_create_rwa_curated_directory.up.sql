-- rwa_curated_directory: a THIRD PARTY'S list of what counts as a tokenized
-- real-world asset on Stellar, and that party's own price for it, cached
-- for the one surface that consults it (/v1/rwa/assets, the curated arm).
--
-- What a row means, precisely: "curator X states that this address is a
-- real-world asset issued by company Y, of subclass Z, and values one
-- token at P dollars." That is a CURATION — an editorial table the curator
-- publishes and maintains — and it is the entire content of the claim. It
-- carries no signature, no proof of control of the address, no on-chain
-- attestation and no market. Rows admitted on it are published under their
-- own basis and their own total, never merged into the verified set.
--
-- The first curator is the Stellar team's public Dune uploads:
-- dune.stellar.dataset_recognized_assets (membership, company, subclass) and
-- dune.stellar.dataset_asset_prices (a daily close_usd per contract). Both
-- are CSV uploads by Dune's own documentation of the dune.<team>.dataset_
-- namespace. Synced by `stellarindex-ops curated-rwa-sync`.
--
-- Two staleness bounds, enforced in the reader's SQL so no caller can
-- forget them: recognition ages on synced_at (our clock); the price ages on
-- priced_at (the curator's own day stamp).
BEGIN;
CREATE TABLE IF NOT EXISTS rwa_curated_directory (
    curator        text        NOT NULL,
    address        text        NOT NULL
                   CHECK (address ~ '^C[A-Z2-7]{55}$'
                       OR address ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'),
    asset_code     text        NOT NULL DEFAULT '',
    asset_issuer   text        NOT NULL DEFAULT '',
    company        text        NOT NULL DEFAULT '',
    asset_subclass text        NOT NULL DEFAULT '',
    price_usd      numeric,
    priced_at      timestamptz,
    source         text        NOT NULL,
    synced_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (curator, address),
    CONSTRAINT rwa_curated_directory_price_has_time CHECK (
        (price_usd IS NULL AND priced_at IS NULL)
     OR (price_usd IS NOT NULL AND priced_at IS NOT NULL)
    )
);
COMMENT ON TABLE rwa_curated_directory IS
    'A third party''s curated list of tokenized real-world assets on Stellar '
    'and that party''s own USD price per token. An editorial table, not an '
    'attestation: no signature, no proof of control, no market. Rows are '
    'served under their own basis and total, never merged into the verified '
    'RWA set. Synced by `stellarindex-ops curated-rwa-sync`.';
COMMENT ON COLUMN rwa_curated_directory.curator IS
    'Which curation this row came from, e.g. "dune:stellar". Part of the key '
    'so two curators may disagree about one address without overwriting.';
COMMENT ON COLUMN rwa_curated_directory.address IS
    'The Stellar address as the curator published it: a Soroban contract '
    'C-strkey (for a classic asset, its Stellar Asset Contract), or a classic '
    'CODE-GISSUER pair. Readers join on it and never on the code.';
COMMENT ON COLUMN rwa_curated_directory.asset_code IS
    'The asset code the curator published beside the address. Display and '
    'reconciliation only; never a key.';
COMMENT ON COLUMN rwa_curated_directory.asset_issuer IS
    'The G-address the curator published as issuer, when it did. Display and '
    'reconciliation only.';
COMMENT ON COLUMN rwa_curated_directory.company IS
    'The curator''s issuing-entity label ("Spiko", "Realiz"). The curator''s '
    'word; served verbatim and labelled as such.';
COMMENT ON COLUMN rwa_curated_directory.asset_subclass IS
    'The curator''s instrument class ("US Treasuries", "Private Credit"). '
    'Served verbatim beside our own vocabulary, never mapped onto it.';
COMMENT ON COLUMN rwa_curated_directory.price_usd IS
    'The curator''s published USD price per token, exactly as printed. '
    'NUMERIC per ADR-0003; carried as a decimal string end to end.';
COMMENT ON COLUMN rwa_curated_directory.priced_at IS
    'The curator''s own stamp for that price (the upload''s day). The price '
    'ages on this clock, not on synced_at.';
COMMENT ON COLUMN rwa_curated_directory.source IS
    'Provenance of the sync run: the tables read and the query used.';
COMMENT ON COLUMN rwa_curated_directory.synced_at IS
    'When this index last saw the row. Recognition ages on this clock; rows '
    'a run did not touch are pruned by that run.';
COMMIT;
