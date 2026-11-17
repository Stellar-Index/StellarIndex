-- 0231 up — `source_exclusions`: operator-declared exclusions of a price
-- source, optionally scoped to one stored pair, over a time window.
--
-- The kill switch for served prices must not write to `trades` (one writer
-- per domain), so an exclusion is a control row that readers apply at read
-- time. Written only by `stellarindex-ops source-exclude`, which appends the
-- audit_log row first. A row is never deleted: lifting it stamps the three
-- lift columns, so the table is the full history of what was excluded when.
--
-- base_asset / quote_asset both NULL scopes the row to every pair of the
-- source. No explicit GRANT: the API role reads it through 0213's default
-- privileges; the aggregator and the ops command connect as the owner.
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
    CONSTRAINT source_exclusions_window_order
        CHECK (until_ts IS NULL OR until_ts > from_ts),
    CONSTRAINT source_exclusions_reason_nonblank CHECK (btrim(reason) <> ''),
    CONSTRAINT source_exclusions_actor_nonblank CHECK (btrim(actor) <> ''),
    CONSTRAINT source_exclusions_lift_all_or_none
        CHECK ((lifted_at IS NULL) = (lifted_by IS NULL)
           AND (lifted_at IS NULL) = (lift_reason IS NULL)),
    CONSTRAINT source_exclusions_lift_reason_nonblank
        CHECK (lift_reason IS NULL OR btrim(lift_reason) <> '')
);

CREATE INDEX source_exclusions_active_idx
    ON source_exclusions (source, from_ts)
    WHERE lifted_at IS NULL;

COMMENT ON TABLE source_exclusions IS
    'Operator exclusions of a price source (optionally one stored pair) from '
    'served prices over [from_ts, until_ts). Written by stellarindex-ops '
    'source-exclude after its audit_log row; lifted rows are stamped, never '
    'deleted. Pair matching across directions and aliases is the reader''s job.';

COMMIT;
