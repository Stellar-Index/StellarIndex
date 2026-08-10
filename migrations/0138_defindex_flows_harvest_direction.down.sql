-- 0138 down: restore the two-direction CHECK. Fails (by design) if
-- harvest rows exist — delete them first if a rollback is truly
-- intended; they are re-derivable via `stellarindex-ops
-- projector-replay -source defindex`.
ALTER TABLE defindex_strategy_flows
    DROP CONSTRAINT defindex_strategy_flows_direction_check;
ALTER TABLE defindex_strategy_flows
    ADD CONSTRAINT defindex_strategy_flows_direction_check
    CHECK (direction IN ('deposit', 'withdraw'));
