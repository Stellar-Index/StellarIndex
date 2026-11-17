-- 0231 up — `source_exclusions`: operator-declared exclusions of a price
-- source, optionally scoped to one pair, over a time window.
--
-- The kill switch for served prices must not write to `trades` (one writer
-- per domain), so an exclusion is a control row that readers apply at read
-- time. Written only by `stellarindex-ops source-exclude`, which appends the
-- audit_log row first. The table is the full history of what was excluded
-- when, so it is append-only: a trigger refuses DELETE and TRUNCATE, and
-- allows an UPDATE only when it is a lift (the three lift columns go from
-- all NULL to all set) that leaves every other column unchanged.
--
-- base_asset / quote_asset both NULL scopes the row to every pair of the
-- source. A set pair is stored in canonical orientation (canonical.Orient);
-- readers MUST match it in both directions and across every
-- canonical.AssetAliases form of each leg (XLM is `native`, `crypto:XLM`
-- and its SAC).
--
-- No explicit GRANT: the API role reads it through 0213's default
-- privileges; the aggregator and the ops command connect as the owner,
-- which is why the append-only rule is a trigger and not a REVOKE (0179).
--
-- Plain table, new and empty: additive, old-binary-safe (rule 9).

BEGIN;

CREATE TABLE source_exclusions (
    id           bigserial    PRIMARY KEY,
    source       text         NOT NULL,
    base_asset   text,
    quote_asset  text,
    from_ts      timestamptz  NOT NULL,
    until_ts     timestamptz,
    reason       text         NOT NULL,
    actor        text         NOT NULL,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    lifted_at    timestamptz,
    lifted_by    text,
    lift_reason  text,

    CONSTRAINT source_exclusions_source_nonblank CHECK (btrim(source) <> ''),
    CONSTRAINT source_exclusions_pair_both_or_neither
        CHECK ((base_asset IS NULL) = (quote_asset IS NULL)),
    CONSTRAINT source_exclusions_pair_nonblank_distinct
        CHECK (base_asset IS NULL
               OR (btrim(base_asset) <> '' AND btrim(quote_asset) <> ''
                   AND base_asset <> quote_asset)),
    CONSTRAINT source_exclusions_window_order
        CHECK (until_ts IS NULL OR until_ts > from_ts),
    CONSTRAINT source_exclusions_reason_nonblank CHECK (btrim(reason) <> ''),
    CONSTRAINT source_exclusions_actor_nonblank CHECK (btrim(actor) <> ''),
    CONSTRAINT source_exclusions_lift_all_or_none
        CHECK ((lifted_at IS NULL) = (lifted_by IS NULL)
           AND (lifted_at IS NULL) = (lift_reason IS NULL)),
    CONSTRAINT source_exclusions_lifted_by_nonblank
        CHECK (lifted_by IS NULL OR btrim(lifted_by) <> ''),
    CONSTRAINT source_exclusions_lift_reason_nonblank
        CHECK (lift_reason IS NULL OR btrim(lift_reason) <> '')
);

CREATE INDEX source_exclusions_unlifted_idx
    ON source_exclusions (source, from_ts)
    WHERE lifted_at IS NULL;

COMMENT ON TABLE source_exclusions IS
    'Operator exclusions of a price source (optionally one pair, stored in '
    'canonical orientation) from served prices over [from_ts, until_ts). '
    'Written by stellarindex-ops source-exclude after its audit_log row. '
    'Append-only: the only permitted UPDATE is a lift. Readers must match the '
    'pair in both directions and across every canonical.AssetAliases form.';

CREATE FUNCTION source_exclusions_append_only() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.lifted_at IS NULL AND OLD.lifted_by IS NULL AND OLD.lift_reason IS NULL
       AND NEW.lifted_at IS NOT NULL AND NEW.lifted_by IS NOT NULL AND NEW.lift_reason IS NOT NULL
       AND to_jsonb(NEW) - 'lifted_at' - 'lifted_by' - 'lift_reason'
           = to_jsonb(OLD) - 'lifted_at' - 'lifted_by' - 'lift_reason'
    THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'source_exclusions is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege',
              HINT = 'The only permitted change is a lift: lifted_at, lifted_by and lift_reason set once, nothing else changed.';
END;
$$;

COMMENT ON FUNCTION source_exclusions_append_only() IS
    'Refuses DELETE and TRUNCATE of source_exclusions, and any UPDATE other '
    'than a one-time lift that leaves every other column unchanged.';

CREATE TRIGGER source_exclusions_append_only  -- migration-compat:ok table created above; no released binary writes it
    BEFORE UPDATE OR DELETE ON source_exclusions
    FOR EACH ROW EXECUTE FUNCTION source_exclusions_append_only();

CREATE TRIGGER source_exclusions_no_truncate  -- migration-compat:ok table created above; no released binary writes it
    BEFORE TRUNCATE ON source_exclusions
    FOR EACH STATEMENT EXECUTE FUNCTION source_exclusions_append_only();

COMMIT;
