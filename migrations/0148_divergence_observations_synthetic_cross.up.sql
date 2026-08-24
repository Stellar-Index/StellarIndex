-- 0148 — admit 'synthetic-usd-cross' to divergence_observations.reference.
--
-- PR #149 adds the synthetic USD-cross reference (base/fiat:X :=
-- oracle base/USD ÷ reflector-fx fiat:X/USD) so EUR/GBP-quoted pairs
-- get a second reference and the ADR-0019 corroborated-release gate
-- can auto-release genuine repricings unattended. The verification
-- panel rejected the code-only change: 0019's CHECK enumerates the
-- reference set, so every flushObservations INSERT for the new source
-- would fail — and the audit-trail row for the very reference that
-- certifies unattended releases could never be written (the table's
-- stated post-mortem purpose). This migration is that panel's fix.
--
-- Same DROP + re-ADD idiom as 0070/0092. divergence_observations is a
-- compressed hypertable (segmentby includes `reference`), so the
-- constraint swap needs the 0101 decompress-every-chunk dance first;
-- the compression policy recompresses the touched chunks on its next
-- run (same posture as 0101 — no manual recompress here).

SELECT decompress_chunk(c, true) FROM show_chunks('divergence_observations') c;

ALTER TABLE divergence_observations
    DROP CONSTRAINT divergence_observations_reference_check;
-- Old-binary safety: this is a pure WIDENING of the dropped CHECK (the
-- old value set is a strict subset), so the previous released binary —
-- which writes only the seven pre-existing reference values, all still
-- admitted — runs unaffected against the new schema. Verified
-- empirically by the PR #149 panel against timescaledb 2.26.4.
ALTER TABLE divergence_observations
    ADD CONSTRAINT divergence_observations_reference_check CHECK (reference IN -- migration-compat:ok pure widening; old value set is a strict subset (see above)
        ('chainlink','coingecko',
         'reflector-cex','reflector-fx','reflector-dex',
         'redstone','band',
         'synthetic-usd-cross'));
