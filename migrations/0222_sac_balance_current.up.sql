-- 0222 up — per-(contract, holder) current SAC balance, kept by a trigger.
--
-- Classic supply sums every holder's latest sac_balance_observations row
-- for an asset. DISTINCT ON over the whole history decompresses and sorts
-- every row the asset ever wrote: 3.5 s per call on the reference
-- deployment, 9.4 s at worst.
--
-- The 0215/0220 design: sac_balance_current holds one row per
-- (asset, contract, holder), the row the full scan would pick, and a row
-- trigger replaces it only when the new key is >= the stored key, so every
-- writer converges with no watermark. The reader seeds an asset on first
-- use and marks it in sac_balance_current_seeded; an asset with no marker
-- is served by the full scan. A delete, a key-lowering update, an update
-- that moves a row to another asset or holder, or a TRUNCATE drops the
-- asset's marker and the next read reseeds. drop_chunks fires no trigger;
-- sac_balance_observations has no retention policy, and adding one must
-- also clear sac_balance_current_seeded.
--
-- Additive; old-binary-safe (rule 9): old binaries keep the full scan and
-- their writes still pass through the trigger.

BEGIN;

CREATE TABLE sac_balance_current (
    asset_key        text        NOT NULL,
    contract_id      text        NOT NULL,
    holder           text        NOT NULL,
    ledger           integer     NOT NULL,
    observed_at      timestamptz NOT NULL,
    walk_version     smallint    NOT NULL,
    intra_ledger_seq bigint      NOT NULL,
    balance_stroops  numeric     NOT NULL,
    is_removal       boolean     NOT NULL,
    PRIMARY KEY (asset_key, contract_id, holder)
);

CREATE TABLE sac_balance_current_seeded (
    asset_key text        PRIMARY KEY,
    seeded_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION sac_balance_current_invalidate(p_asset text, p_contract text, p_holder text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('sac_balance_current:' || p_asset, 0));
    DELETE FROM sac_balance_current_seeded WHERE asset_key = p_asset;
    DELETE FROM sac_balance_current
     WHERE asset_key = p_asset AND contract_id = p_contract AND holder = p_holder;
END
$$;

CREATE FUNCTION sac_balance_current_track()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM sac_balance_current_invalidate(OLD.asset_key, OLD.contract_id, OLD.holder);
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND (
        (NEW.asset_key, NEW.contract_id, NEW.holder) IS DISTINCT FROM (OLD.asset_key, OLD.contract_id, OLD.holder)
        OR (NEW.ledger, NEW.walk_version, NEW.intra_ledger_seq, NEW.observed_at)
         < (OLD.ledger, OLD.walk_version, OLD.intra_ledger_seq, OLD.observed_at)
    ) THEN
        PERFORM sac_balance_current_invalidate(OLD.asset_key, OLD.contract_id, OLD.holder);
    END IF;
    INSERT INTO sac_balance_current AS c (
        asset_key, contract_id, holder, ledger, observed_at,
        walk_version, intra_ledger_seq, balance_stroops, is_removal
    ) VALUES (
        NEW.asset_key, NEW.contract_id, NEW.holder, NEW.ledger, NEW.observed_at,
        NEW.walk_version, NEW.intra_ledger_seq, NEW.balance_stroops, NEW.is_removal
    )
    ON CONFLICT (asset_key, contract_id, holder) DO UPDATE SET
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

CREATE FUNCTION sac_balance_current_truncate()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE sac_balance_current, sac_balance_current_seeded;
    RETURN NULL;
END
$$;

CREATE TRIGGER sac_balance_current_track
    AFTER INSERT OR UPDATE OR DELETE ON sac_balance_observations
    FOR EACH ROW EXECUTE FUNCTION sac_balance_current_track();

CREATE TRIGGER sac_balance_current_truncate
    AFTER TRUNCATE ON sac_balance_observations
    FOR EACH STATEMENT EXECUTE FUNCTION sac_balance_current_truncate();

COMMIT;
