package v1_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The /revenue body over a fixed dataset (priced, carried, out-of-bound, source-excluded, thin and unpriced hours across
// two days, hourly and daily) is pinned byte for byte, so refactoring its valuation cannot change what it serves.
// STELLARINDEX_UPDATE_GOLDEN=1 rewrites the file.
func TestLendingPoolRevenue_GoldenBody(t *testing.T) {
	pool, xlm, thin, dark := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 5), mkCStrkey(t, 7)
	thinAsset, err := canonical.ParseAsset(thin)
	if err != nil {
		t.Fatal(err)
	}
	h := func(n int) time.Time {
		return time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	}
	hours := func(gross int64, at ...int) []clickhouse.LendingRevenueBucket {
		var out []clickhouse.LendingRevenueBucket
		for i, n := range at {
			s := clickhouse.NewLendingRevenueSums()
			s.Gross.SetInt64(gross * int64(i+1))
			s.Backstop.SetInt64(gross * int64(i+1) / 5)
			s.SupplierIntended.Sub(s.Gross, s.Backstop)
			s.SupplierRealised.Set(s.SupplierIntended)
			s.InterestDrawn.SetInt64(-int64(i))
			out = append(out, clickhouse.LendingRevenueBucket{Start: h(n), Sums: s})
		}
		return out
	}
	xh, th, dh := hours(12_345_678, 1, 2, 5, 10, 21, 27, 30), hours(10_000_000, 3, 26), hours(7, 4)
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	prices := pairPrices(map[string]map[time.Time]string{
		"native/fiat:USD": {h(1): "0.1", h(2): "0.3", h(20): "0.25", h(26): "0.2"},
		canonical.Pair{Base: thinAsset, Quote: usd}.String(): {h(3): "2", h(26): "3"},
	})
	for _, interval := range []string{"1h", "1d"} {
		fold := func(b []clickhouse.LendingRevenueBucket) []clickhouse.LendingRevenueBucket {
			if interval == "1d" {
				return clickhouse.FoldLendingRevenueDays(b)
			}
			return b
		}
		v2 := blend.PoolV2
		srv := v1.New(v1.Options{
			Explorer: &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
				Covered: true, FloorLedger: 1, FloorTime: h(-1), ThroughLedger: 50, ThroughHour: h(40), From: h(0), Before: h(41),
				Reserves: []clickhouse.LendingRevenueReserve{
					{Asset: xlm, Hours: xh, Buckets: fold(xh)},
					{Asset: thin, Hours: th, Buckets: fold(th)},
					{Asset: dark, Hours: dh, Buckets: fold(dh), StaleRows: 2},
				},
			}},
			Lending: &stubLendingReader{version: &v2}, History: prices, TokenDecimals: &lockstepDecStub{d: 7, found: true},
			Substance: pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAsset.String(): true}}, pricingguard.SubstanceGateOptions{}),
			Exclusions: assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
				ID: 1, Source: "soroswap", Base: "native", Quote: "fiat:USD", From: h(20), Until: h(21),
			}}}),
		})
		rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/revenue?interval="+interval+"&from=2027-03-01T00:00:00Z&to=2027-03-03T00:00:00Z")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", interval, rec.Code, rec.Body)
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		got, err := json.MarshalIndent(map[string]json.RawMessage{"data": env["data"], "flags": env["flags"]}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", "lending_revenue_golden_"+interval+".json")
		if os.Getenv("STELLARINDEX_UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s body differs from %s:\n%s", interval, path, got)
		}
	}
}
