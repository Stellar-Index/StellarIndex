-- 0215 up — per-holder current trustline balance, kept by a trigger.
--
-- Classic supply sums every holder's latest trustline_observations row
-- for an asset. Doing that with DISTINCT ON over the whole history reads
-- every row the asset ever wrote (KALE: 73.7M rows for 12.5k holders),
-- 20-28 s per call, and it was the top Postgres cost on the reference
-- deployment.
--
-- trustline_balance_current holds one row per (asset, holder): the row
-- the full scan would pick, ordered (ledger, walk_version,
-- intra_ledger_seq, observed_at). A row trigger on trustline_observations
-- keeps it: an insert or update replaces the stored row only when its
-- key is >= the stored key. Keep-the-highest is order-independent, so
-- late, out-of-order, backfill and seed writes all converge with no
-- watermark, and the trigger covers every writer, old binaries included.
--
-- Rows written before this migration are not in the table. The reader
-- seeds an asset on first use (same keep-the-highest upsert, strict >),
-- then marks it in trustline_balance_current_seeded. An asset with no
-- marker is served by the full scan, as before.
--
-- A delete, a key-lowering update or a TRUNCATE on trustline_observations
-- cannot be undone by keep-the-highest, so it drops the asset's marker
-- (and the holder's row) and the next read reseeds. Deletes and seeds
-- serialise on a per-asset advisory lock so a seed cannot mark an asset
-- whose holder row a concurrent delete removed. drop_chunks fires no
-- trigger; trustline_observations has no retention policy, and adding
-- one must also clear trustline_balance_current_seeded.
--
-- Additive; old-binary-safe (rule 9): old binaries keep the full scan
-- and their writes still pass through the trigger.

BEGIN;

CREATE TABLE trustline_balance_current (
    asset_key        text        NOT NULL,
    account_id       text        NOT NULL,
    ledger           integer     NOT NULL,
    observed_at      timestamptz NOT NULL,
    walk_version     smallint    NOT NULL,
    intra_ledger_seq bigint      NOT NULL,
    balance_stroops  numeric     NOT NULL,
    is_removal       boolean     NOT NULL,
    PRIMARY KEY (asset_key, account_id)
);

CREATE TABLE trustline_balance_current_seeded (
    asset_key text        PRIMARY KEY,
    seeded_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION trustline_balance_current_invalidate(p_asset text, p_account text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('trustline_balance_current:' || p_asset, 0));
    DELETE FROM trustline_balance_current_seeded WHERE asset_key = p_asset;
    DELETE FROM trustline_balance_current WHERE asset_key = p_asset AND account_id = p_account;
END
$$;

CREATE FUNCTION trustline_balance_current_track()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM trustline_balance_current_invalidate(OLD.asset_key, OLD.account_id);
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND (
        (NEW.asset_key, NEW.account_id) IS DISTINCT FROM (OLD.asset_key, OLD.account_id)
        OR (NEW.ledger, NEW.walk_version, NEW.intra_ledger_seq, NEW.observed_at)
         < (OLD.ledger, OLD.walk_version, OLD.intra_ledger_seq, OLD.observed_at)
    ) THEN
        PERFORM trustline_balance_current_invalidate(OLD.asset_key, OLD.account_id);
    END IF;
    INSERT INTO trustline_balance_current AS c (
        asset_key, account_id, ledger, observed_at,
        walk_version, intra_ledger_seq, balance_stroops, is_removal
    ) VALUES (
        NEW.asset_key, NEW.account_id, NEW.ledger, NEW.observed_at,
        NEW.walk_version, NEW.intra_ledger_seq, NEW.balance_stroops, NEW.is_removal
    )
    ON CONFLICT (asset_key, account_id) DO UPDATE SET
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

CREATE FUNCTION trustline_balance_current_truncate()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE trustline_balance_current, trustline_balance_current_seeded;
    RETURN NULL;
END
$$;

CREATE TRIGGER trustline_balance_current_track
    AFTER INSERT OR UPDATE OR DELETE ON trustline_observations
    FOR EACH ROW EXECUTE FUNCTION trustline_balance_current_track();

CREATE TRIGGER trustline_balance_current_truncate
    AFTER TRUNCATE ON trustline_observations
    FOR EACH STATEMENT EXECUTE FUNCTION trustline_balance_current_truncate();

COMMIT;
