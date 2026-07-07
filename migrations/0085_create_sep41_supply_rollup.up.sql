-- 0085 up — `sep41_supply_rollup`: incremental per-contract running
-- mint/burn/clawback checkpoint for the SEP-41 Algorithm-3 supply read.
--
-- Why this exists (incident 2026-07-06). The reader that feeds the
-- aggregator's SEP-41 supply refresher — timescale.Store
-- .SEP41KindTotalsAtOrBefore — computed
--     Σ amount FILTER (WHERE event_kind='mint') / 'burn' / 'clawback'
-- over ALL of a contract's `sep41_supply_events` rows on EVERY tick.
-- That table's original design assumption ("watched-set restricted →
-- grows slowly", migration 0015) was broken by the 2026-07-05
-- full-history re-derive, which grew it to hundreds of millions of
-- rows. `sep41_supply_events` is a hypertable chunked by `observed_at`,
-- but the query bounds only `contract_id` + `ledger` — so it can prune
-- NO chunks and index-scans every 7-day chunk, aggregating the whole
-- per-contract history each call. Three watched contracts refreshing
-- concurrently ran three minutes-long full aggregates in parallel,
-- saturated Postgres IO, and blew up API p95/p99 (the API shares the
-- same Postgres served tier). This is the OLTP-for-OLAP anti-pattern
-- ADR-0034 warns against.
--
-- The fix (ADR-0034 spirit, kept in the served tier for exact parity).
-- A tiny rollup table — ONE row per watched contract — holds the
-- per-kind running totals folded up to `last_ledger`. A periodic worker
-- (cmd/stellarindex-aggregator) advances it INCREMENTALLY: each pass
-- sums only the rows with `ledger > last_ledger` (a bounded tail on the
-- (contract_id, ledger DESC) index — cheap) and adds them in. The
-- reader returns `rollup + Σ(delta above last_ledger, up to asOfLedger)`
-- — a sargable tail sum, never the full history — and falls back to the
-- original full sum only when no rollup row exists yet or the request
-- ledger predates the checkpoint (rare historical/backfill reads).
--
-- Semantics preserved EXACTLY: the rollup sums the same rows the full
-- query did (rollup ⊕ delta = full aggregate ≤ asOfLedger). No
-- ClickHouse round-trip, so no event-set / decode-shape divergence
-- risk (the PG observer is watched-set-gated and bare-i128-only; the
-- CH supply_flows lake is network-wide and map-variant-aware — their
-- per-contract totals can legitimately differ, so we do NOT re-source
-- the aggregator's snapshot from CH).
--
-- i128 discipline (ADR-0003): the three totals are NUMERIC, never a
-- fixed-width int — Σmint alone can exceed i128. Go reads them as
-- decimal ::text → *big.Int.
--
-- Watermark safety: `last_ledger` only advances over SETTLED ledgers
-- (the worker folds `ledger < max(ledger)`, never the current tip
-- ledger which may still be mid-write in the indexer). The reader's
-- live delta covers everything above the checkpoint up to the request
-- ledger, so a conservatively-lagging checkpoint never loses events.
-- A `sep41_supply_events` re-derive/backfill that rewrites history
-- below an existing checkpoint requires a `TRUNCATE sep41_supply_rollup`
-- so the worker re-folds from zero (documented alongside the re-derive).
--
-- Old-binary-safe: purely additive — a new standalone table, no
-- existing table/column/policy touched. A pre-0085 binary keeps using
-- the full-sum path (the reader falls back when the row is absent);
-- a post-0085 binary populates + reads the rollup.

CREATE TABLE sep41_supply_rollup (
    -- SEP-41 contract C-strkey — one row per watched contract.
    contract_id    text        PRIMARY KEY,

    -- Per-kind running totals folded up to `last_ledger`, NUMERIC per
    -- ADR-0003 (i128 sums never truncate to a fixed width). Always
    -- non-negative — each is a sum of non-negative event amounts;
    -- direction is discriminated by kind at read time.
    mint_total     numeric     NOT NULL DEFAULT 0 CHECK (mint_total >= 0),
    burn_total     numeric     NOT NULL DEFAULT 0 CHECK (burn_total >= 0),
    clawback_total numeric     NOT NULL DEFAULT 0 CHECK (clawback_total >= 0),

    -- Highest ledger folded into the totals above. The reader sums the
    -- live delta over (last_ledger, asOfLedger]; the worker folds the
    -- next delta over (last_ledger, max_settled_ledger).
    last_ledger    integer     NOT NULL DEFAULT 0 CHECK (last_ledger >= 0),

    updated_at     timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE sep41_supply_rollup IS
    'Incremental per-contract mint/burn/clawback checkpoint for SEP-41 '
    'Algorithm-3 supply. Advanced by the aggregator rollup worker; read '
    'as rollup + sargable delta by SEP41KindTotalsAtOrBefore. Prevents '
    'the full-history per-tick aggregate over sep41_supply_events '
    '(incident 2026-07-06).';
COMMENT ON COLUMN sep41_supply_rollup.last_ledger IS
    'Highest SETTLED ledger folded into the totals; the reader adds the '
    'live delta above it up to the request ledger.';
