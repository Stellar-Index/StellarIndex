-- 0216 — admit 'chainlink-stellar' to divergence_observations.reference.
--
-- The divergence worker compares against Chainlink's DataFeedsCache on Stellar under the
-- reference name 'chainlink-stellar', but 0148's CHECK only admits the retired EVM
-- 'chainlink', so every one of its observation INSERTs failed and the audit trail for that
-- reference was never written.
--
-- Same DROP + re-ADD idiom and decompress step as 0148 (compressed hypertable, segmentby
-- includes `reference`); the compression policy recompresses the touched chunks.

SELECT decompress_chunk(c, true) FROM show_chunks('divergence_observations') c;

ALTER TABLE divergence_observations
    DROP CONSTRAINT divergence_observations_reference_check;
-- Old-binary safety: a pure WIDENING of the dropped CHECK, so the previous release's writes
-- are all still admitted.
ALTER TABLE divergence_observations
    ADD CONSTRAINT divergence_observations_reference_check CHECK (reference IN -- migration-compat:ok pure widening; old value set is a strict subset (see above)
        ('chainlink','chainlink-stellar','coingecko',
         'reflector-cex','reflector-fx','reflector-dex',
         'redstone','band',
         'synthetic-usd-cross'));
