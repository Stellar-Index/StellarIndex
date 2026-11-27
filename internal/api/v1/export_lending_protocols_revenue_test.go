package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ProtocolRevenueForTest folds a one-pool BLNT-shaped block from cov and sums, its verdicts read through s exactly as the
// protocols route reads them.
func (s *Server) ProtocolRevenueForTest(ctx context.Context, cov timescale.LendingRevenueUSDCoverage, sums []timescale.LendingRevenueUSDBucketSum) LendingProtocolRevenue {
	return foldProtocolRevenue(protocolRevenueInput{
		pools: []revenuePool{{pool: cov.Pool}}, covs: map[string]timescale.LendingRevenueUSDCoverage{cov.Pool: cov},
		sums: sums, verdicts: s.revenueVerdicts(ctx, sums),
	})
}
