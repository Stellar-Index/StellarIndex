-- 0216 down — restore 0148's CHECK. Refuses while 'chainlink-stellar' rows exist: delete them
-- explicitly first (same stance as 0148's down).

SELECT decompress_chunk(c, true) FROM show_chunks('divergence_observations') c;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM divergence_observations WHERE reference = 'chainlink-stellar') THEN
    RAISE EXCEPTION '0216_divergence_observations_chainlink_stellar.down.sql: divergence_observations still holds rows where reference = ''chainlink-stellar''. Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE divergence_observations
    DROP CONSTRAINT divergence_observations_reference_check;
ALTER TABLE divergence_observations
    ADD CONSTRAINT divergence_observations_reference_check CHECK (reference IN
        ('chainlink','coingecko',
         'reflector-cex','reflector-fx','reflector-dex',
         'redstone','band',
         'synthetic-usd-cross'));
