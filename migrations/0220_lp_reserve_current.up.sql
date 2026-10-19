-- 0220 up — per-pool current LP reserve, kept by a trigger.
--
-- Classic supply sums every pool's latest lp_reserve_observations row for
-- an asset. DISTINCT ON over the whole history decompresses and sorts every
-- row the asset ever wrote (USDC: 6.8M rows for 1,039 pools, a 700 MB disk
-- sort), 5.1 s mean per call on the reference deployment.
--
-- The same design as 0215, which keeps trustline_balance_current:
-- lp_reserve_current holds one row per (asset, pool), the row the full scan
-- would pick, and a row trigger replaces it only when the new key is >= the
-- stored key, so every writer converges with no watermark. The reader seeds
-- an asset on first use and marks it in lp_reserve_current_seeded; an asset
-- with no marker is served by the full scan. A delete, a key-lowering update
-- or a TRUNCATE drops the asset's marker and the next read reseeds.
-- drop_chunks fires no trigger; lp_reserve_observations has no retention
-- policy, and adding one must also clear lp_reserve_current_seeded.
--
-- Additive; old-binary-safe (rule 9): old binaries keep the full scan and
-- their writes still pass through the trigger.

BEGIN;

CREATE TABLE lp_reserve_current (
    asset_key        text        NOT NULL,
    pool_id       text        NOT NULL,
    ledger           integer     NOT NULL,
    observed_at      timestamptz NOT NULL,
    walk_version     smallint    NOT NULL,
    intra_ledger_seq bigint      NOT NULL,
    balance_stroops  numeric     NOT NULL,
    is_removal       boolean     NOT NULL,
    PRIMARY KEY (asset_key, pool_id)
);

CREATE TABLE lp_reserve_current_seeded (
    asset_key text        PRIMARY KEY,
    seeded_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION lp_reserve_current_invalidate(p_asset text, p_pool text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('lp_reserve_current:' || p_asset, 0));
    DELETE FROM lp_reserve_current_seeded WHERE asset_key = p_asset;
    DELETE FROM lp_reserve_current WHERE asset_key = p_asset AND pool_id = p_pool;
END
$$;

CREATE FUNCTION lp_reserve_current_track()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM lp_reserve_current_invalidate(OLD.asset_key, OLD.pool_id);
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND (
        (NEW.asset_key, NEW.pool_id) IS DISTINCT FROM (OLD.asset_key, OLD.pool_id)
        OR (NEW.ledger, NEW.walk_version, NEW.intra_ledger_seq, NEW.observed_at)
         < (OLD.ledger, OLD.walk_version, OLD.intra_ledger_seq, OLD.observed_at)
    ) THEN
        PERFORM lp_reserve_current_invalidate(OLD.asset_key, OLD.pool_id);
    END IF;
    INSERT INTO lp_reserve_current AS c (
        asset_key, pool_id, ledger, observed_at,
        walk_version, intra_ledger_seq, balance_stroops, is_removal
    ) VALUES (
        NEW.asset_key, NEW.pool_id, NEW.ledger, NEW.observed_at,
        NEW.walk_version, NEW.intra_ledger_seq, NEW.balance_stroops, NEW.is_removal
    )
    ON CONFLICT (asset_key, pool_id) DO UPDATE SET
        ledger           = EXCLUDED.ledger,
        observed_at      = EXCLUDED.observed_at,
        walk_version     = EXCLUDED.walk_version,
        intra_ledger_seq = EXCLUDED.intra_ledger_seq,
        balance_stroops  = EXCLUDED.balance_stroops,
        is_removal       = EXCLUDED.is_removal
    WHERE (c.ledger, c.walk_version, c.intra_ledger_seq, c.observed_at)
       <= (EXCLUDED.ledger, EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq, EXCLUDED.observed_at);
    RETURN NULL;
END
$$;

CREATE FUNCTION lp_reserve_current_truncate()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE lp_reserve_current, lp_reserve_current_seeded;
    RETURN NULL;
END
$$;

CREATE TRIGGER lp_reserve_current_track
    AFTER INSERT OR UPDATE OR DELETE ON lp_reserve_observations
    FOR EACH ROW EXECUTE FUNCTION lp_reserve_current_track();

CREATE TRIGGER lp_reserve_current_truncate
    AFTER TRUNCATE ON lp_reserve_observations
    FOR EACH STATEMENT EXECUTE FUNCTION lp_reserve_current_truncate();

COMMIT;
