-- 0186 up — record when each reference observed the price it was compared on.
--
-- A divergence_observations row carried observed_at, the comparison time,
-- and nothing about the reference's own quote. The worker already refuses a
-- quote older than divergence.MaxComparableAge (1h, or the FX liveness
-- budget for fiat/fiat), but inside that ceiling a quote seconds old and one
-- fifty minutes old were indistinguishable on the row and on /v1/divergence,
-- so a post-mortem could not tell a real divergence from a reference that
-- was describing an earlier market (GH #823).
--
-- ref_observed_at is the quote's AsOf: the oracle round's updatedAt, the
-- CoinGecko id's last_updated_at, the on-chain oracle's ledger close time.
--
-- Additive and old-binary-safe (README rule 9): the column is NULLABLE with
-- no default. The previous binary INSERTs without it and keeps working;
-- rows written before this migration, or by that binary, stay NULL, which
-- is the truth — their reference time was never recorded. The table is a
-- compressed hypertable; a nullable column with no default is added to
-- compressed chunks in place, with no decompression (0119 is the
-- precedent, exercised against a compressed chunk in
-- test/integration/freeze_ladder_durability_test.go).

ALTER TABLE divergence_observations
    ADD COLUMN IF NOT EXISTS ref_observed_at timestamptz;

COMMENT ON COLUMN divergence_observations.ref_observed_at IS
    'When the reference''s upstream observed ref_price (the quote''s as-of: '
    'oracle round time, CoinGecko last_updated_at, ledger close time). '
    'observed_at is the comparison time; observed_at - ref_observed_at is '
    'the reference''s age at comparison, at most divergence.MaxComparableAge. '
    'NULL on rows written before 0186 or by a binary that predates it.';
