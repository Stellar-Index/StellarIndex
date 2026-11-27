package v1_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// revenueStore is the Postgres side of the revenue block: a coverage map and the groups it returns for any spans.
type revenueStore struct {
	covs    map[string]timescale.LendingRevenueUSDCoverage
	sums    []timescale.LendingRevenueUSDBucketSum
	failSum bool
	spans   []timescale.LendingRevenueUSDSpan
}

func (*revenueStore) CountRecentEventsBySource(context.Context) (map[string]int64, error) {
	return nil, nil
}

func (r *revenueStore) LendingRevenueUSDCoverages(context.Context) (map[string]timescale.LendingRevenueUSDCoverage, error) {
	return r.covs, nil
}

func (r *revenueStore) LendingRevenueUSDBucketSums(_ context.Context, spans []timescale.LendingRevenueUSDSpan, _, _ int16, _ string) ([]timescale.LendingRevenueUSDBucketSum, error) {
	r.spans = spans
	if r.failSum {
		return nil, errors.New("pg down")
	}
	return r.sums, nil
}

func revenueGroup(pool, asset string, bucket int, leg string) timescale.LendingRevenueUSDBucketSum {
	g := timescale.LendingRevenueUSDBucketSum{Pool: pool, Asset: asset, Bucket: bucket, Current: true, Days: 1}
	for i := range g.Legs {
		g.Legs[i] = &leg
	}
	return g
}

func strOr(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// Every host today: no reader rows, no coverage. The block is present on both protocols with null amounts, never zeros.
func TestLendingProtocols_RevenueNeverRunIsNull(t *testing.T) {
	for name, store := range map[string]v1.ProtocolStatsReader{"no reader": nil, "empty table": &revenueStore{}} {
		t.Run(name, func(t *testing.T) {
			f := newProtocolsServer(t, protocolsCase{clean: true, revenue: store})
			stats, _ := getProtocols(t, f.srv)
			for _, st := range stats {
				w := st.Revenue.AllTime
				if w.InterestAccruedUSD != nil || w.BackstopTakeUSD != nil || strOr(w.USDReason) != "not_valued" || !w.LowerBound || len(w.Excluded) == 0 {
					t.Fatalf("%s all_time = %+v; want null amounts, not_valued, a named lower bound", st.Protocol, w)
				}
				for _, e := range w.Excluded {
					if e.Reason != "not_valued" {
						t.Fatalf("%s: exclusion %+v; want not_valued", st.Protocol, e)
					}
				}
				if (st.Protocol == "blnt") != st.Revenue.FeeRateInferred || (st.Protocol == "blnt") != (w.Versions == nil) {
					t.Fatalf("%s: fee_rate_inferred %v, versions %+v", st.Protocol, st.Revenue.FeeRateInferred, w.Versions)
				}
			}
		})
	}
}

// A covered Blend pool's stored days reach the block per window and per version; the BLNT pool, uncovered, stays null;
// a failed sums read names every pool and the response is not cached as good.
func TestLendingProtocols_RevenueFromStoredDays(t *testing.T) {
	through := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	store := &revenueStore{
		covs: map[string]timescale.LendingRevenueUSDCoverage{
			f0.activePool: {Pool: f0.activePool, FloorDay: through.AddDate(0, 0, -100), ThroughDay: through},
			f0.frozenPool: {Pool: f0.frozenPool, FloorDay: through.AddDate(0, 0, -100), ThroughDay: through},
		},
		sums: []timescale.LendingRevenueUSDBucketSum{
			revenueGroup(f0.activePool, f0.xlmSAC, 0, "1.25"), revenueGroup(f0.activePool, f0.xlmSAC, 3, "10"),
			revenueGroup(f0.frozenPool, f0.xlmSAC, 1, "2.5"),
		},
	}
	f := newProtocolsServer(t, protocolsCase{clean: true, revenue: store, v1Pool: func(f ratesFixture) string { return f.frozenPool }})
	stats, _ := getProtocols(t, f.srv)
	blend, blnt := stats[0].Revenue, stats[1].Revenue
	if strOr(blend.Day.InterestAccruedUSD) != "1.25" || strOr(blend.Week.InterestAccruedUSD) != "3.75" || strOr(blend.AllTime.InterestAccruedUSD) != "13.75" {
		t.Fatalf("blend 1d/7d/all = %s/%s/%s; want 1.25/3.75/13.75", strOr(blend.Day.InterestAccruedUSD),
			strOr(blend.Week.InterestAccruedUSD), strOr(blend.AllTime.InterestAccruedUSD))
	}
	if blend.AllTime.LowerBound || strOr(blend.AllTime.ThroughDay) != "2026-11-20" {
		t.Fatalf("blend all_time lower %v through %s excluded %+v", blend.AllTime.LowerBound, strOr(blend.AllTime.ThroughDay), blend.AllTime.Excluded)
	}
	if v := blend.Week.Versions; v == nil || strOr(v.V1.InterestAccruedUSD) != "2.50" || strOr(v.V2.InterestAccruedUSD) != "1.25" {
		t.Fatalf("blend 7d versions = %+v; want v1 2.50, v2 1.25", v)
	}
	if blnt.Day.InterestAccruedUSD != nil || strOr(blnt.Day.USDReason) != "not_valued" {
		t.Fatalf("uncovered BLNT 1d = %+v; want null, not_valued", blnt.Day)
	}

	store.failSum = true
	failed := newProtocolsServer(t, protocolsCase{clean: true, revenue: store})
	stats, resp := getProtocols(t, failed.srv)
	assertProtocolsNoStore(t, resp, true)
	w := stats[0].Revenue.AllTime
	if w.InterestAccruedUSD != nil || len(w.Excluded) == 0 || w.Excluded[0].Reason != "revenue_read_failed" {
		t.Fatalf("failed read served %+v", w)
	}
}
