-- 0197 up — compress a trades chunk only once it is older than the
-- asset-character roll's 14-day window.
--
-- WHY: the 6-hourly roll (timescale.Store.RefreshAssetVolumeCharacter) reads
-- 14 days of trades for ~23 min under ACCESS SHARE. compress_chunk needs
-- ACCESS EXCLUSIVE at the end to truncate the old heap, so with
-- compress_after = 7 days the chunk aged 7-14 days fell due while the roll
-- held it: the policy timed out and a 52 GB chunk sat uncompressed. 15 days
-- is the window plus a day for a roll that started before the chunk fell
-- due. The roll bounds trades with an interval literal, so it locks only the
-- chunks inside its window; with a bind parameter it locked every chunk.
--
-- Cost: about one more week of trades stays uncompressed (~50 GB on r1).
--
-- Rejected: timescaledb.compress_truncate_behaviour = truncate_or_delete. On
-- a busy chunk it DELETEs instead of truncating, and a session whose cached
-- plan predates the compression then reads 0 rows from that chunk.
--
-- Lock: alter_job rewrites one catalog row; no table lock. Old-binary-safe:
-- the previous roll still locks every chunk, so the policy can still miss a
-- run until the new binary is deployed; nothing is written differently.

BEGIN;

DO $$
DECLARE
    n int;
BEGIN
    PERFORM alter_job(j.job_id,
                      config => jsonb_set(j.config, '{compress_after}', to_jsonb('15 days'::text)))
       FROM timescaledb_information.jobs j
      WHERE j.proc_name = 'policy_compression'
        AND j.hypertable_schema = current_schema()
        AND j.hypertable_name = 'trades';
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n <> 1 THEN
        RAISE EXCEPTION '0197: expected exactly one trades compression policy, found %', n;
    END IF;
END
$$;

COMMIT;
