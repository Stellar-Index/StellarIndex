package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// scamReserve is a classic asset whose issuer is on the curated scam list.
const scamReserve = "SCAM-GA2XZLXNLAL26VBCA2OESAIMXTRH5GXKLHYZMDGNCR2SYS5QZWWNBLCK"

// Three reserves no live reserves view holds: XLM (clear), a token whose latest market is thin, and one whose issuer is
// flagged. The revenue block refuses exactly the reserves /revenue refuses for the same prices and day, names them
// withheld, and sums neither.
func TestProtocolRevenue_VerdictsMatchRevenueRoute(t *testing.T) {
	pool, xlm, thin := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 5)
	thinAsset, err := canonical.ParseAsset(thin)
	if err != nil {
		t.Fatal(err)
	}
	scamAsset, err := canonical.ParseAsset(scamReserve)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return day.Add(time.Duration(n) * time.Hour) }
	hours := func() []clickhouse.LendingRevenueBucket {
		var out []clickhouse.LendingRevenueBucket
		for _, n := range []int{1, 2} {
			s := clickhouse.NewLendingRevenueSums()
			s.Gross.SetInt64(10_000_000)
			s.SupplierIntended.Set(s.Gross)
			s.SupplierRealised.Set(s.Gross)
			out = append(out, clickhouse.LendingRevenueBucket{Start: h(n), Sums: s})
		}
		return out
	}
	assets := []string{xlm, thin, scamReserve}
	var reserves []clickhouse.LendingRevenueReserve
	for _, a := range assets {
		b := hours()
		reserves = append(reserves, clickhouse.LendingRevenueReserve{Asset: a, Buckets: b, Hours: b})
	}
	ex := &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
		Covered: true, FloorLedger: 1, FloorTime: h(-1), ThroughLedger: 50, ThroughHour: h(5), From: h(0), Before: h(4), Reserves: reserves,
	}}
	prices := map[time.Time]string{h(1): "0.1", h(2): "0.3"}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	v2 := blend.PoolV2
	srv := v1.New(v1.Options{
		Explorer: ex, Lending: &stubLendingReader{version: &v2},
		History: pairPrices(map[string]map[time.Time]string{
			"native/fiat:USD": prices, canonical.Pair{Base: thinAsset, Quote: usd}.String(): prices,
			canonical.Pair{Base: scamAsset, Quote: usd}.String(): prices,
		}),
		TokenDecimals: &lockstepDecStub{d: 7, found: true},
		Substance:     pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAsset.String(): true}}, pricingguard.SubstanceGateOptions{}),
	})

	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/revenue?interval=1h&from=2027-03-01T00:00:00Z&to=2027-03-01T04:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	data := lendingUSDData(t, rec.Body.Bytes())
	var route []string
	for _, a := range assets {
		var u struct {
			Reason *string `json:"usd_reason"`
		}
		if err := json.Unmarshal(reserveNamed(t, data["reserves"], a)["usd"], &u); err != nil {
			t.Fatal(err)
		}
		if u.Reason != nil && *u.Reason == "withheld" {
			route = append(route, a)
		}
	}

	var sums []timescale.LendingRevenueUSDBucketSum
	for _, a := range assets {
		g := revenueGroup(pool, a, 0, "1")
		g.LastAmountDay = day
		sums = append(sums, g)
	}
	cov := timescale.LendingRevenueUSDCoverage{Pool: pool, FloorDay: day.AddDate(0, 0, -10), ThroughDay: day}
	block := srv.ProtocolRevenueForTest(context.Background(), cov, sums)
	var withheld []string
	for _, e := range block.AllTime.Excluded {
		if e.Reason == "withheld" && e.Asset != nil {
			withheld = append(withheld, *e.Asset)
		}
	}
	slices.Sort(route)
	slices.Sort(withheld)
	want := []string{scamReserve, thin}
	slices.Sort(want)
	if !slices.Equal(route, want) || !slices.Equal(withheld, want) {
		t.Fatalf("/revenue withholds %v, the block %v; want both %v", route, withheld, want)
	}
	for _, w := range []v1.LendingProtocolRevenueWindow{block.Day, block.AllTime} {
		if strOr(w.InterestAccruedUSD) != "1.00" || !w.LowerBound {
			t.Fatalf("window %s lower %v; want only XLM's 1.00, a lower bound", strOr(w.InterestAccruedUSD), w.LowerBound)
		}
	}
	if block.Degraded {
		t.Fatal("verdict refusals degraded the block")
	}
}
