-- 0051 up — `ledger_ingest_log` substrate-continuity record (ADR-0033 Phase 2).
--
-- One row per ledger we have fully processed, written POST-persist
-- (after the ledger's events land in their per-source tables), so the
-- record is an authoritative "this ledger is done" marker — unlike the
-- ledgerstream cursor, which advances BEFORE persistence and so can
-- claim a ledger a mid-write panic actually lost.
--
-- This table is the foundation of the three-claim completeness model:
--
--   Claim 1 (Substrate) — the set of ledger_seq is contiguous from a
--   source's genesis to its watermark, AND the hash chain is unbroken
--   (prev_ledger_hash[N] == ledger_hash[N-1]). Both are cheap queries
--   over THIS narrow table — never an unbounded trades scan
--   (feedback_no_unbounded_trade_scan).
--
-- The two census counts are computed at ingest directly from the
-- LedgerCloseMeta, WITHOUT decoding event bodies (internal/dispatcher
-- CensusLedger). They are the LCM's own ground truth — the EIP-7792
-- pattern: the LCM is the committed source, the count is the checksum.
--
--   - soroban_event_count       — contract events eligible for capture
--     (Type=Contract, ContractId set, body V=0, ≥1 topic), from
--     successful txs. This MUST equal COUNT(soroban_events WHERE
--     ledger=N); a shortfall is a capture/persistence gap (Claim 3).
--   - classic_trade_effect_count — sum of ClaimAtoms across successful
--     trade ops (ManageSell/BuyOffer, CreatePassiveSellOffer,
--     PathPaymentStrict{Receive,Send}). Mirrors exactly how SDEX
--     produces one canonical.Trade per ClaimAtom, so it MUST equal
--     COUNT(trades WHERE source='sdex' AND ledger=N) (ADR-0033 Phase 5).
--
-- Once a ledger is in this table with its census, "zero rows for
-- contract C at ledger N" is a PROVEN quiet period, not a maybe-gap —
-- which is what lets the confidence signal stop guessing sparsity
-- thresholds (ADR-0033 demotes MinGapSizeOverride to alerting only).
--
-- Shape: a plain table keyed by ledger_seq (bigint PK). The dominant
-- queries are ledger_seq-range continuity (LAG window) and the
-- hash-chain self-join (a.ledger_seq = b.ledger_seq - 1) — both want
-- the btree on ledger_seq, not a time partition. ~62M narrow rows
-- (~6 GB) is comfortable against the 13.85 TB pool; convert to a
-- hypertable later if compression is wanted.
--
-- Retention: NONE — this is the permanent completeness ledger.

BEGIN;

CREATE TABLE ledger_ingest_log (
    ledger_seq                 bigint       PRIMARY KEY CHECK (ledger_seq >= 0),
    ledger_close_time          timestamptz  NOT NULL,

    -- 32-byte LCM header hashes. ledger_hash is this ledger's hash;
    -- prev_ledger_hash is header.PreviousLedgerHash. The hash-chain
    -- check is prev_ledger_hash[N] = ledger_hash[N-1].
    ledger_hash                bytea        NOT NULL,
    prev_ledger_hash           bytea        NOT NULL,

    -- LCM-derived census (decoder-independent). See header comment.
    soroban_event_count        integer      NOT NULL CHECK (soroban_event_count >= 0),
    classic_trade_effect_count integer      NOT NULL CHECK (classic_trade_effect_count >= 0),

    -- When we wrote this row (post-persist). Distinct from
    -- ledger_close_time (when the network closed the ledger).
    persisted_at               timestamptz  NOT NULL DEFAULT now()
);

COMMENT ON TABLE ledger_ingest_log IS
    'Substrate-continuity record (ADR-0033). One row per fully-'
    'processed ledger, written post-persist. soroban_event_count / '
    'classic_trade_effect_count are LCM-derived checksums reconciled '
    'against soroban_events / trades. Contiguity + hash-chain over '
    'this table is Claim 1 of the completeness model.';

-- Time-range lookups (status page, "what did we process in window W").
CREATE INDEX ledger_ingest_log_close_time_idx
    ON ledger_ingest_log (ledger_close_time);

COMMIT;
