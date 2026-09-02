-- 0148 down — restore the seven-reference CHECK (pre-synthetic-cross).
-- Synthetic rows must be deleted EXPLICITLY first — this down now REFUSES if any exist
-- (deliberate: down-migrating with data present should be loud, not
-- silent — same stance as 0092/0070's downs). Same decompress dance as
-- the up (compressed hypertable; DELETE + constraint swap both need
-- uncompressed chunks). No explicit BEGIN — matches the 0101/0092
-- decompress-migration convention (the runner owns transactionality).

SELECT decompress_chunk(c, true) FROM show_chunks('divergence_observations') c;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM divergence_observations WHERE reference = 'synthetic-usd-cross') THEN
    RAISE EXCEPTION '0148_divergence_observations_synthetic_cross.down.sql: divergence_observations still holds rows where reference = ''synthetic-usd-cross'' — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE divergence_observations
    DROP CONSTRAINT divergence_observations_reference_check;
ALTER TABLE divergence_observations
    ADD CONSTRAINT divergence_observations_reference_check CHECK (reference IN
        ('chainlink','coingecko',
         'reflector-cex','reflector-fx','reflector-dex',
         'redstone','band'));
