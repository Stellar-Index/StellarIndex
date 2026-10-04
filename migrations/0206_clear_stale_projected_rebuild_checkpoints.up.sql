-- 0206 up — clear the projected-rebuild checkpoints that 0137 and 0164
-- left behind for the tables they emptied, and that 0203 left for the
-- table it added.
--
-- `stellarindex-ops projected-rebuild` records each finished window as an
-- ingestion_cursors row (source = 'projected-rebuild',
-- sub_source = '<source>:<from>-<to>') and, with -resume (the default),
-- skips every window that already has one. It never checks that the target
-- table still holds the window's rows. 0137 emptied comet_liquidity and
-- 0164 emptied cctp_events and rozo_events without deleting those rows, so
-- a rebuild of comet, cctp or rozo over a window checkpointed before the
-- migration skips it and the table stays empty there. r1 hit this for
-- cctp/rozo on 2026-09-29 and the rows were deleted by hand. 0203 added
-- sushiswap_v3_position_events to a source that already projected swaps,
-- so a sushiswap_v3 window checkpointed before 0203 is skipped the same way.
--
-- Deleting the rows only costs re-work: a re-run is idempotent because the
-- projected tables' upserts are ON CONFLICT DO UPDATE guarded by
-- derive_generation. A cctp, rozo or sushiswap_v3 re-run writes into the
-- trades path, so decompress trades chunks first, as 0203's follow-up says.
-- The live projector's own cursors
-- (source = 'projector') and every other source's checkpoints are left
-- alone. The ':' after the name keeps 'comet:%' from matching another
-- source whose name starts with "comet". '_' is a LIKE wildcard, but no
-- other source name fits 'sushiswap?v3:'.
--
-- A rebuild that is running while this applies read its checkpoints at
-- start, so it is not affected; one started afterwards re-walks these
-- windows. Lock: row locks on ingestion_cursors only. Old-binary-safe: an
-- older binary reads a missing checkpoint as "window not done".

BEGIN;

-- The guard refuses only if the DELETE itself removed a row outside
-- projected-rebuild (the predicate makes that impossible). It looks at the
-- deleted rows, not at table counts, so a live writer adding or removing a
-- cursor while this runs cannot trip it. Re-running is a no-op once the
-- matching checkpoints are gone.
DO $$
DECLARE
    stray bigint;
BEGIN
    WITH d AS (
        DELETE FROM ingestion_cursors
         WHERE source = 'projected-rebuild'
           AND (sub_source LIKE 'comet:%'
                OR sub_source LIKE 'cctp:%'
                OR sub_source LIKE 'rozo:%'
                OR sub_source LIKE 'sushiswap_v3:%')
        RETURNING source
    )
    SELECT count(*) FILTER (WHERE source <> 'projected-rebuild') INTO stray FROM d;
    IF stray > 0 THEN
        RAISE EXCEPTION '0206: deleted % ingestion_cursors rows outside projected-rebuild; refusing', stray;
    END IF;
END
$$;

COMMIT;
