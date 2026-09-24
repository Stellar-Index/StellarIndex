-- 0176 down — restore 0003's original comment on oracle_updates
-- verbatim (0003_create_oracle_updates_hypertable.up.sql:51-53), so
-- `migrate down` leaves pg_description byte-identical to the pre-0176
-- state.

COMMENT ON TABLE oracle_updates IS
    'Every observed oracle publication, one row per (source, ledger, tx_hash, op_index). '
    'Hypertable partitioned on ts. See ADR-0006.';
