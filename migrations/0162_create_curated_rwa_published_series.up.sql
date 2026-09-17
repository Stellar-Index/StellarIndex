-- curated_rwa_published_series: the totals a THIRD-PARTY curator PUBLISHES
-- about tokenized real-world assets on Stellar, cached for the RWA surface's
-- curated arm (/v1/rwa/assets `curated.published`).
--
-- Why a second curated table beside rwa_curated_directory (0161). That table
-- was built to hold the curator's per-asset list and per-asset prices, read
-- from the two CSV uploads the "RWAs on Stellar" dashboard values Stellar's
-- balances against. Those uploads are PRIVATE to the uploading team: Dune
-- refuses `dune.stellar.dataset_recognized_assets` and
-- `dune.stellar.dataset_asset_prices` to every outside account ("Uploaded
-- table ... does not exist or it is private"), so no sync can load a row of
-- them. What the curator does let anyone read is the latest RESULT of the
-- dashboard's public queries — a monthly RWA market-cap total, and that
-- total split by the curator's own asset-subclass labels. This table holds
-- exactly those, as published, and nothing derived from them.
--
-- What a row means, precisely: "curator X's public query Q, when it last
-- ran at executed_at, printed value_usd for series S at month_end (and
-- subclass, for the split)." That is the curator's arithmetic over the
-- curator's private inputs. It carries no signature, no per-asset
-- breakdown, no price and no market; it is served under its own name,
-- beside the verified set and never inside it, so a reader can see the
-- curator's headline and this index's verified total on one page and the
-- signed gap between them.
--
-- Series: one row per month for the total ("rwa_mcap_monthly", subclass
-- ''), one row per (month, subclass) for the split
-- ("rwa_mcap_monthly_by_subclass"). The sync REPLACES a curator's rows per
-- series in one transaction, so a series is always one execution's whole
-- output and never a splice of two. Every row of one run shares
-- observed_at; the reader's recognition bound ages on it (our clock), the
-- same fail-closed shape as 0160/0161.
BEGIN;
CREATE TABLE IF NOT EXISTS curated_rwa_published_series (
    curator      text        NOT NULL,
    series       text        NOT NULL,
    month_end    date        NOT NULL,
    subclass     text        NOT NULL DEFAULT '',
    value_usd    numeric     NOT NULL,
    source_query bigint      NOT NULL,
    executed_at  timestamptz NOT NULL,
    observed_at  timestamptz NOT NULL,
    PRIMARY KEY (curator, series, month_end, subclass)
);
COMMENT ON TABLE curated_rwa_published_series IS
    'The totals a third-party curator publishes about tokenized real-world '
    'assets on Stellar — a monthly market-cap series and its split by the '
    'curator''s own subclass labels — read from the latest result of the '
    'curator''s PUBLIC queries, because its per-asset list and prices are '
    'private. The curator''s arithmetic over the curator''s inputs: no '
    'signature, no per-asset breakdown, no market. Served under its own name '
    'beside the verified set, never inside it. Synced by `stellarindex-ops '
    'curated-rwa-sync`.';
COMMENT ON COLUMN curated_rwa_published_series.curator IS
    'Which curation this row came from, e.g. "dune:stellar". Part of the key '
    'so two curators may publish different figures without overwriting.';
COMMENT ON COLUMN curated_rwa_published_series.series IS
    'Which published series: "rwa_mcap_monthly" (one row per month, '
    'subclass '''') or "rwa_mcap_monthly_by_subclass" (one row per month '
    'and subclass). Replaced whole, per series, on every sync.';
COMMENT ON COLUMN curated_rwa_published_series.month_end IS
    'The month the curator bucketed the figure into, as the last day of that '
    'month, exactly as the query printed it.';
COMMENT ON COLUMN curated_rwa_published_series.subclass IS
    'The curator''s asset-subclass label ("US Treasuries"), verbatim; empty '
    'on the total series. Never mapped onto this index''s vocabulary.';
COMMENT ON COLUMN curated_rwa_published_series.value_usd IS
    'The USD figure the curator''s query printed, exactly as printed. '
    'NUMERIC per ADR-0003; carried as a decimal string end to end.';
COMMENT ON COLUMN curated_rwa_published_series.source_query IS
    'The curator''s public query id the row was read from, so a reader can '
    'open the same result on the curator''s site.';
COMMENT ON COLUMN curated_rwa_published_series.executed_at IS
    'When the curator''s query last ran — the curator''s clock. The figure '
    'is as fresh as this, not as fresh as observed_at.';
COMMENT ON COLUMN curated_rwa_published_series.observed_at IS
    'When this index read the result. The reader''s recognition bound ages '
    'on this clock; every row of one sync run shares it.';
COMMIT;
