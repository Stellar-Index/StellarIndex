-- 0148 down — restore the seven-reference CHECK (pre-synthetic-cross).
-- Synthetic rows must be deleted first or the constraint re-add fails
-- (deliberate: down-migrating with data present should be loud, not
-- silent — same stance as 0092/0070's downs). Same decompress dance as
-- the up (compressed hypertable; DELETE + constraint swap both need
-- uncompressed chunks). No explicit BEGIN — matches the 0101/0092
-- decompress-migration convention (the runner owns transactionality).

SELECT decompress_chunk(c, true) FROM show_chunks('divergence_observations') c;

DELETE FROM divergence_observations WHERE reference = 'synthetic-usd-cross';

ALTER TABLE divergence_observations
    DROP CONSTRAINT divergence_observations_reference_check;
ALTER TABLE divergence_observations
    ADD CONSTRAINT divergence_observations_reference_check CHECK (reference IN
        ('chainlink','coingecko',
         'reflector-cex','reflector-fx','reflector-dex',
         'redstone','band'));
