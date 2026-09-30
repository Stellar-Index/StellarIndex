-- 0193 up — create fx_fixings (vendor-time FX bars: hourly where Massive has them, daily below)
--
-- A closed derived fiat price must convert at an FX rate that is a pure
-- function of the bucket it prices, identical in every region and on every
-- re-read (ADR-0018). fx_quotes cannot be that series: it is daily, keyed on
-- the day, and rewritten in place for a trailing week by each region's own
-- forex worker. This table is keyed on the VENDOR's bar time instead, so two
-- regions that fetch the same closed bar store the same row.
--
-- Rows are append-only. A writer inserts and never updates; a correction is a
-- new row at generation max+1 for the same (ticker, grain, bar_start), and
-- readers take the highest generation. ingested_at is audit only: it sits in
-- no key and no read predicate, so a region's write time never enters an
-- answer.
--
-- rate_usd is units of <ticker> per 1 USD (the C:USD<ticker> close), the
-- fx_quotes orientation, stored as NUMERIC from the vendor's decimal text.
--
-- No retention: the series is the audit trail of every served conversion.
-- New table only: the previous binary neither reads nor writes it (rule 9).

BEGIN;

CREATE TABLE fx_fixings (
    ticker       text        NOT NULL,
    grain        text        NOT NULL CHECK (grain IN ('1h', '1d')),
    -- The vendor's bar start (`t`), verbatim.
    bar_start    timestamptz NOT NULL,
    -- bar_start + 1 hour or + 1 day: the instant the close was fixed.
    bar_end      timestamptz NOT NULL,
    rate_usd     numeric     NOT NULL CHECK (rate_usd > 0),
    source       text        NOT NULL,
    generation   bigint      NOT NULL DEFAULT 0,
    ingested_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (bar_end > bar_start),
    -- A daily bar and the midnight hourly bar share bar_start.
    PRIMARY KEY (ticker, grain, bar_start, generation)
);

SELECT create_hypertable(
    'fx_fixings',
    'bar_start',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

-- The at-or-before binding walks one ticker's bars newest-first by bar_end.
CREATE INDEX fx_fixings_ticker_bar_end_idx
    ON fx_fixings (ticker, bar_end DESC);

ALTER TABLE fx_fixings SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'ticker',
    timescaledb.compress_orderby   = 'bar_end DESC'
);

SELECT add_compression_policy('fx_fixings', INTERVAL '90 days');

COMMENT ON TABLE fx_fixings IS
    'Vendor-time FX bars (units of ticker per 1 USD), hourly where Massive has '
    'them and daily below; append-only, corrections at a higher generation.';

COMMIT;
