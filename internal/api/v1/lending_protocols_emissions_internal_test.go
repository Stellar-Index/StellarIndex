package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A read-time refusal of the emission token (withheld, or price_read_failed when its verdict cannot be read) leaves the
// token's stored USD out of every stream, named with its rows; its tokens are still counted.
func TestFoldProtocolEmissions_ReadTimeVerdictRefusesStoredUSD(t *testing.T) {
	through := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	token, decimals, usd := "CTOKEN", 7, "4"
	verified := map[string]historyVerdict{}
	for _, s := range protocolEmissionStreams {
		verified[s] = historyVerified
	}
	for _, verdict := range []string{exclusionPriceWithheld, exclusionPriceReadFailed} {
		t.Run(verdict, func(t *testing.T) {
			em := foldProtocolEmissions(protocolEmissionsInput{
				pools: []emisPool{{pool: "P"}},
				covs: map[string]timescale.LendingEmissionsUSDCoverage{"P": {
					Pool: "P", FloorDay: through.AddDate(0, 0, -9), ThroughDay: through, ValuationRev: lendingEmissionsValuationRev,
				}},
				sums: []timescale.LendingEmissionsUSDBucketSum{{
					Pool: "P", Stream: emisRowPoolClaims, Current: true, EmissionToken: &token, EmissionTokenDecimals: &decimals,
					Amount: "8", AmountUSD: &usd, Rows: 2,
				}},
				verdicts: map[string]string{token: verdict},
				history:  verified,
			})
			pc := em.Day.Streams.PoolClaims
			if pc.AmountUSD != nil || pc.USDReason == nil || *pc.USDReason != protocolRevenueAllExcluded || !pc.LowerBound {
				t.Fatalf("pool_claims = %+v; want its USD refused, never summed", pc)
			}
			if pc.Amount == nil || *pc.Amount != "8" || pc.AmountLowerBound {
				t.Fatalf("pool_claims amount = %+v; want 8 tokens, exact", pc)
			}
			if len(em.Day.Excluded) != 1 || em.Day.Excluded[0].Reason != verdict || em.Day.Excluded[0].Count == nil || *em.Day.Excluded[0].Count != 2 {
				t.Fatalf("excluded %+v; want %s for 2 rows", em.Day.Excluded, verdict)
			}
			if em.Degraded != (verdict == exclusionPriceReadFailed) {
				t.Fatalf("degraded %v for %s", em.Degraded, verdict)
			}
		})
	}
}
