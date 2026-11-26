package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// gatedRouteExclusionProof names, per gated census row, the tests that drive that route with a source exclusion in force
// and assert it withholds. A function value, so a row cannot cite a test that does not exist.
var gatedRouteExclusionProof = map[string][]func(*testing.T){
	"GET /v1/price":                             {TestPriceSourceExclusion_RedisLayer, TestFXSourceExclusionWithholdsTheFXLeg},
	"GET /v1/price/batch":                       {TestPriceSourceExclusion_RedisLayer},
	"POST /v1/price/batch":                      {TestPriceSourceExclusion_BatchPost},
	"GET /v1/price/tip":                         {TestPriceSourceExclusion_TipWindow},
	"GET /v1/price/tip/stream":                  {TestPriceSourceExclusion_TipStream},
	"GET /v1/price/stream":                      {TestPriceSourceExclusion_ClosedStream},
	"GET /v1/vwap":                              {TestVWAPAndTWAPDropExcludedTrades},
	"GET /v1/twap":                              {TestVWAPAndTWAPDropExcludedTrades},
	"GET /v1/ohlc":                              {TestOHLCSeriesWithholdsExcludedBars},
	"GET /v1/oracle/lastprice":                  {TestPriceSourceExclusion_SEP40FrozenHeldValue},
	"GET /v1/oracle/prices":                     {TestPriceSourceExclusion_SEP40FrozenHeldValue},
	"GET /v1/oracle/x_last_price":               {TestPriceSourceExclusion_SEP40FrozenHeldValue},
	"GET /v1/lending/pools/{pool}/liquidations": {TestLendingPoolLiquidations_ExcludedSourceDoesNotValueFill},
	"GET /v1/rwa/premium":                       {TestRWAPremium_ASourceExcludedDayIsWithheldAndFlagged},
	"GET /v1/lending/pools/{pool}/history":      {TestLendingHistory_ExcludedSourceDoesNotValueBucket},
	"GET /v1/lending/pools/{pool}/revenue":      {TestLendingPoolRevenue_ExcludedSourceDoesNotValueHour},
	"GET /v1/assets/{asset_id}":                 {TestAssetDetail_SourceExclusionWithholdsCataloguePrice, v1.TestAssetRoutes_ExclusionGatesEveryVariant},
	"GET /v1/price/at":                          {TestPriceAtWithholdsExcludedBucket, TestPriceAtJudgesBucketSources},
	"GET /v1/price/changes":                     {TestPriceChangesWithholdsExcludedHorizon, TestPriceChangesJudgesBucketSources},
	"GET /v1/history":                           {TestHistoryPagesPastExcludedFills},
	"GET /v1/history/since-inception":           {TestChartSeriesWithholdsExcludedBuckets},
	"GET /v1/chart":                             {TestChartSeriesWithholdsExcludedBuckets},
	"GET /v1/assets":                            {TestAssetListing_SourceExclusionWithholdsCoveredRows, v1.TestAssetRoutes_ExclusionGatesEveryVariant},
	"GET /v1/assets/verified":                   {TestGlobalAssetFXSourceExclusionWithholdsFiatRate},
	"GET /v1/external/assets/{slug}":            {TestGlobalAssetFXSourceExclusionWithholdsFiatRate},

	"GET /v1/lending/pools/{pool}/reserves/{asset}/history": {TestLendingHistory_ExcludedSourceDoesNotValueBucket},
}

// TestPriceRouteCoverage_GatedRoutesHaveExclusionProof: a route filed gated must cite a behavioural exclusion test, and a
// proof row must name a route the census still files gated.
func TestPriceRouteCoverage_GatedRoutesHaveExclusionProof(t *testing.T) {
	gated := map[string]bool{}
	for _, op := range v1.GatedPriceRoutes() {
		gated[op] = true
		if len(gatedRouteExclusionProof[op]) == 0 {
			t.Errorf("%s is filed gated with no exclusion test in gatedRouteExclusionProof", op)
		}
	}
	for op := range gatedRouteExclusionProof {
		if !gated[op] {
			t.Errorf("gatedRouteExclusionProof lists %s, which the census does not file gated", op)
		}
	}
}
