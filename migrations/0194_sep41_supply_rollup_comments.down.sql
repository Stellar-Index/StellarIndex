-- 0194 down — restore 0085's original sep41_supply_rollup comments. Local/dev
-- iteration only (README rule 9): the text is metadata, nothing else moves.

COMMENT ON TABLE sep41_supply_rollup IS
    'Incremental per-contract mint/burn/clawback checkpoint for SEP-41 '
    'Algorithm-3 supply. Advanced by the aggregator rollup worker; read '
    'as rollup + sargable delta by SEP41KindTotalsAtOrBefore. Prevents '
    'the full-history per-tick aggregate over sep41_supply_events '
    '(incident 2026-07-06).';

COMMENT ON COLUMN sep41_supply_rollup.last_ledger IS
    'Highest SETTLED ledger folded into the totals; the reader adds the '
    'live delta above it up to the request ledger.';
