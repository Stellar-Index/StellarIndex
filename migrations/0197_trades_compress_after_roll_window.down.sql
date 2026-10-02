-- 0197 down — back to 0001's compress_after = 7 days.

BEGIN;

DO $$
DECLARE
    n int;
BEGIN
    PERFORM alter_job(j.job_id,
                      config => jsonb_set(j.config, '{compress_after}', to_jsonb('7 days'::text)))
       FROM timescaledb_information.jobs j
      WHERE j.proc_name = 'policy_compression'
        AND j.hypertable_schema = current_schema()
        AND j.hypertable_name = 'trades';
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n <> 1 THEN
        RAISE EXCEPTION '0197 down: expected exactly one trades compression policy, found %', n;
    END IF;
END
$$;

COMMIT;
