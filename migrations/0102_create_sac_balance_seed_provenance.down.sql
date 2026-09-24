-- 0102 down — drop sac_balance_seed_provenance.
--
-- Supply figures are unaffected: this table is a pure audit trail, never
-- read by the supply computation itself (sac_balance_observations /
-- SumSACBalancesAtOrBefore are untouched). It is not inert under a binary
-- that writes it, though: the SAC balance seed op still seeds balances,
-- then fails its final provenance upsert with 42P01 (undefined_table) and
-- exits non-zero. Revert the code first, or re-create the table before the
-- next seed; a re-seed after re-creating it starts the audit trail fresh.

DROP TABLE IF EXISTS sac_balance_seed_provenance;
