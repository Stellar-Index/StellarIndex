-- 0103 up — broaden `discovered_assets` for the generic-oracle
-- discovery extension (docs/architecture/generic-oracle-sep-onboarding.md
-- §3(b), option (b) recommendation: extend the existing SEP-41
-- sighting-only sniffer rather than build a curated read-adapter).
--
-- internal/canonical/discovery gained two new sniffers:
--   - SniffOracleEvent: a broader oracle-suggestive topic[0] symbol
--     set (the exact 2026-07-10 ClickHouse lake census list).
--   - SniffOracleCall: an event-less-oracle sniffer on the
--     ContractCallContext path (the Band pattern — relay/force_relay
--     update storage without publishing an event), matching on
--     InvokeContract function name.
--
-- Both reuse discovered_assets rather than a parallel table — same
-- "record a sighting, never attribute" discipline the SEP-41 sniffer
-- already has. Two schema changes:
--
--   1. discovery_kind: NEW column distinguishing which sniffer
--      produced the first sighting ('sep41' | 'oracle_event' |
--      'oracle_call'). Defaults to 'sep41' so every pre-existing row
--      (and any old-binary INSERT that doesn't know about the
--      column) reads back as the value it always implicitly was.
--   2. first_seen_event: the CHECK constraint is widened (DROP +
--      re-ADD, same idiom as 0070/0092/0094) to admit the
--      oracle-suggestive symbol set alongside the original four
--      SEP-41 values. The column keeps its name — it now means
--      "first observed symbol/function name," not strictly "SEP-41
--      event" — documented via COMMENT ON COLUMN rather than a
--      rename, since a rename has no functional benefit here and
--      the existing name is still accurate for the (still dominant)
--      SEP-41 case.
--
-- Additive + old-binary-safe per migrations/README.md rule 9: the
-- previous binary only ever wrote the four SEP-41 values (still
-- valid under the widened constraint) and never references
-- discovery_kind (DEFAULT fills it in). Only the new binary writes
-- the broader symbol set / non-default discovery_kind.

BEGIN;

ALTER TABLE discovered_assets
    ADD COLUMN discovery_kind TEXT NOT NULL DEFAULT 'sep41'
        CHECK (discovery_kind IN ('sep41', 'oracle_event', 'oracle_call'));

COMMENT ON COLUMN discovered_assets.discovery_kind IS
    'Which sniffer produced the first sighting: sep41 (internal/canonical/discovery.Sniff, the original four-symbol topic sniffer), oracle_event (SniffOracleEvent, broader oracle-suggestive topic set), or oracle_call (SniffOracleCall, event-less-oracle ContractCallContext path). First-write-wins, same as first_seen_*.';

ALTER TABLE discovered_assets DROP CONSTRAINT discovered_assets_first_seen_event_check;
ALTER TABLE discovered_assets ADD CONSTRAINT discovered_assets_first_seen_event_check CHECK (first_seen_event IN (
    -- Original SEP-41 four.
    'transfer',
    'mint',
    'burn',
    'clawback',
    -- Oracle-suggestive topic[0] / function-name set — exact list
    -- from docs/architecture/generic-oracle-sep-onboarding.md §2's
    -- ClickHouse census (also internal/canonical/discovery's
    -- oracleEventSymbols/oracleCallFunctions maps; the latter is a
    -- subset of the former, so this single list covers both).
    'price',
    'prices',
    'lastprice',
    'last_price',
    'x_last_price',
    'set_price',
    'update_price',
    'price_update',
    'new_price',
    'oracle',
    'Oracle',
    'ORACLE',
    'feed',
    'PriceData',
    'resolution',
    'write_prices',
    'relay',
    'force_relay',
    'REFLECTOR',
    'REDSTONE',
    'rate',
    'rates',
    'set_rate',
    'symbol_rates',
    'StandardReference',
    'update',
    'base',
    'decimals',
    'assets'
));

COMMENT ON COLUMN discovered_assets.first_seen_event IS
    'Which topic/function symbol first surfaced this contract. For discovery_kind=sep41: transfer|mint|burn|clawback. For discovery_kind IN (oracle_event, oracle_call): one of the oracle-suggestive symbols in internal/canonical/discovery (e.g. price_update, relay, REDSTONE) — see the CHECK constraint for the full enumerated set.';

COMMIT;
