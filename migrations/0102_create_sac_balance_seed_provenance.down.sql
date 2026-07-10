-- 0102 down — drop sac_balance_seed_provenance.
--
-- Correctness-safe: this table is a pure audit trail, never read by the
-- supply computation itself (sac_balance_observations / SumSACBalances
-- AtOrBefore are untouched). Dropping it only removes the "when/how was
-- this contract last seeded" record; a re-seed after re-creating the
-- table starts the audit trail fresh.

DROP TABLE IF EXISTS sac_balance_seed_provenance;
