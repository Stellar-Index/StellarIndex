package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CurrentRevenueCoverage is c stamped with the valuer's and the derive's current revisions.
func CurrentRevenueCoverage(c timescale.LendingRevenueUSDCoverage) timescale.LendingRevenueUSDCoverage {
	c.ValuationRev, c.InterestDeriveRev = lendingRevenueValuationRev, int16(lendinginterest.DeriveRev) //nolint:gosec // a small revision number
	return c
}

// ProtocolRevenueForTest folds a one-pool BLNT-shaped block from cov and sums, its verdicts read through s exactly as the
// protocols route reads them.
func (s *Server) ProtocolRevenueForTest(ctx context.Context, cov timescale.LendingRevenueUSDCoverage, sums []timescale.LendingRevenueUSDBucketSum) LendingProtocolRevenue {
	return foldProtocolRevenue(protocolRevenueInput{
		pools: []revenuePool{{pool: cov.Pool}}, covs: map[string]timescale.LendingRevenueUSDCoverage{cov.Pool: cov},
		sums: sums, verdicts: s.revenueVerdicts(ctx, sums),
	})
}
