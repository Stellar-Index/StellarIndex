// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// countingScaleReader counts single and batched scale reads; declared maps contract id to scale.
type countingScaleReader struct {
	declared        map[string]uint32
	batchErr        error
	singles, batchN atomic.Int32
	batchIDs        atomic.Int32
}

func (c *countingScaleReader) TokenDecimals(_ context.Context, id string) (uint32, bool, error) {
	c.singles.Add(1)
	d, ok := c.declared[id]
	return d, ok, nil
}

func (c *countingScaleReader) TokenDecimalsBatch(_ context.Context, ids []string) (map[string]uint32, error) {
	c.batchN.Add(1)
	c.batchIDs.Add(int32(len(ids)))
	if c.batchErr != nil {
		return nil, c.batchErr
	}
	out := map[string]uint32{}
	for _, id := range ids {
		if d, ok := c.declared[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func syntheticContractID(t *testing.T, n int) string {
	t.Helper()
	var h [32]byte
	h[0], h[1], h[31] = byte(n>>8), byte(n), 0xC5
	id, err := strkey.Encode(strkey.VersionByteContract, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func sorobanRows(t *testing.T, n int) []AssetDetail {
	t.Helper()
	rows := make([]AssetDetail, n)
	for i := range rows {
		rows[i] = AssetDetail{AssetID: syntheticContractID(t, i), Type: string(canonical.AssetSoroban), Decimals: 7}
	}
	return rows
}

// A page of 300 Soroban rows resolves its scales in one read, not one per row.
func TestListingScales_SorobanPageIsOneRead(t *testing.T) {
	rows := sorobanRows(t, 300)
	stub := &countingScaleReader{declared: map[string]uint32{rows[0].AssetID: 18}}
	s := &Server{Options: Options{TokenDecimals: stub}}

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if b, n := stub.batchN.Load(), stub.singles.Load(); b != 1 || n != 0 {
		t.Fatalf("reads: %d batched, %d single; want 1 batched, 0 single", b, n)
	}
	if got := stub.batchIDs.Load(); got != 300 {
		t.Errorf("batch carried %d contracts, want 300", got)
	}
	if rows[0].Decimals != 18 || rows[0].scaleUnknown() {
		t.Errorf("declared row: decimals=%d unknown=%v, want 18 known", rows[0].Decimals, rows[0].scaleUnknown())
	}
	if !rows[1].DecimalsUndeclared || rows[1].DecimalsUnresolved {
		t.Errorf("undeclared row: undeclared=%v unresolved=%v, want undeclared only", rows[1].DecimalsUndeclared, rows[1].DecimalsUnresolved)
	}
}

// A failed batch is an unread, not "declares none": every row is unresolved, the page reports the unread, and the next build reads again.
func TestListingScales_FailedReadIsUnresolvedAndNotKept(t *testing.T) {
	stub := &countingScaleReader{batchErr: context.DeadlineExceeded}
	s := &Server{Options: Options{TokenDecimals: stub}, logger: discardLogger()}
	for build := 1; build <= 2; build++ {
		rows := sorobanRows(t, 3)
		unread := s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
		if !unread {
			t.Errorf("build %d: unread not reported", build)
		}
		for i := range rows {
			if !rows[i].DecimalsUnresolved || rows[i].DecimalsUndeclared {
				t.Errorf("build %d row %d: unresolved=%v undeclared=%v, want unresolved only", build, i, rows[i].DecimalsUnresolved, rows[i].DecimalsUndeclared)
			}
		}
		if got := stub.batchN.Load(); got != int32(build) {
			t.Errorf("build %d: %d reads, want %d (a failed read must not be reused)", build, got, build)
		}
	}
}

// A classic row is 7 by protocol and costs no scale read.
func TestListingScales_ClassicRowKeepsSevenWithoutARead(t *testing.T) {
	stub := &countingScaleReader{}
	s := &Server{Options: Options{TokenDecimals: stub}}
	rows := []AssetDetail{{AssetID: "USDC-" + alcIssuer, Type: string(canonical.AssetClassic), Decimals: 7}}

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if n := stub.batchIDs.Load() + stub.singles.Load(); n != 0 {
		t.Errorf("classic row caused %d scale reads, want 0", n)
	}
	b, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"decimals":7`) || rows[0].scaleUnknown() || rows[0].MarketCapDecimalsMismatch {
		t.Errorf("classic row: want decimals 7, known, no mismatch: %s", b)
	}
}

func listingMismatchCount() float64 {
	return testutil.ToFloat64(obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("asset_listing", ""))
}

// A nonstandard lake scale is resolver drift only on a row whose price could have been normalised on the other scale.
func TestListingScales_MismatchOnlyOnValuedRows(t *testing.T) {
	price := "1.5"
	for _, tc := range []struct {
		name  string
		price *string
		want  bool
	}{
		{name: "unpriced", want: false},
		{name: "priced", price: &price, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := sorobanRows(t, 1)
			rows[0].PriceUSD = tc.price
			s := &Server{Options: Options{TokenDecimals: &countingScaleReader{declared: map[string]uint32{rows[0].AssetID: 18}}}}
			before := listingMismatchCount()

			s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

			if rows[0].Decimals != 18 {
				t.Errorf("decimals = %d, want the read 18", rows[0].Decimals)
			}
			if rows[0].MarketCapDecimalsMismatch != tc.want {
				t.Errorf("MarketCapDecimalsMismatch = %v, want %v", rows[0].MarketCapDecimalsMismatch, tc.want)
			}
			if got := listingMismatchCount() - before; (got > 0) != tc.want {
				t.Errorf("mismatch counter moved by %v, want moved=%v", got, tc.want)
			}
		})
	}
}

// A projection-confirmed scale the lake cannot answer is served alike by the listing and by the /v1/rwa contract arm.
func TestContractDecimals_ConfirmedScaleMatchesTheListing(t *testing.T) {
	for name, reader := range map[string]TokenDecimalsReader{
		"undeclared":  undeclaredTokenDecimals{},
		"read failed": failingTokenDecimals{},
	} {
		t.Run(name, func(t *testing.T) {
			id := syntheticContractID(t, 7)
			s := &Server{Options: Options{TokenDecimals: reader, NonstandardDecimals: decimalsCacheFlagging(t, map[string]int{id: 6})}, logger: discardLogger()}

			listing := []AssetDetail{{AssetID: id, Type: string(canonical.AssetSoroban), Decimals: 7}}
			s.fillMarketCapsFromSupply(context.Background(), listing, map[string]int{})
			contract := []AssetDetail{{AssetID: id, Type: string(canonical.AssetSoroban), Decimals: 7}}
			scaled, _ := s.fillContractDecimals(context.Background(), contract, map[string]timescale.AssetRow{id: {}})
			refuseUnscaledContractCaps(contract, scaled)

			l, c := rwaDecimalsOf(listing[0]), rwaDecimalsOf(contract[0])
			if l == nil || c == nil || *l != 6 || *c != 6 {
				t.Errorf("listing decimals=%v, rwa contract decimals=%v; want 6 on both", l, c)
			}
		})
	}
}

// A contract row the listing fill already answered is not read a second time, and a fresh "declares none" clears an earlier unread.
func TestContractDecimals_SettledRowIsNotReadTwice(t *testing.T) {
	rows := sorobanRows(t, 2)
	stub := &countingScaleReader{declared: map[string]uint32{rows[0].AssetID: 9}}
	s := &Server{Options: Options{TokenDecimals: stub}, logger: discardLogger()}
	src := map[string]timescale.AssetRow{rows[0].AssetID: {}, rows[1].AssetID: {}}

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
	scaled, cut := s.fillContractDecimals(context.Background(), rows, src)

	if n := stub.batchN.Load() + stub.singles.Load(); n != 1 {
		t.Errorf("%d scale reads across both fills, want 1", n)
	}
	if _, ok := scaled[rows[0].AssetID]; !ok || cut || rows[0].Decimals != 9 {
		t.Errorf("declared row: scaled=%v cut=%v decimals=%d, want scaled 9", ok, cut, rows[0].Decimals)
	}
	if _, ok := scaled[rows[1].AssetID]; ok || !rows[1].DecimalsUndeclared {
		t.Errorf("undeclared row: scaled=%v undeclared=%v, want unscaled and undeclared", ok, rows[1].DecimalsUndeclared)
	}

	fresh := []AssetDetail{{AssetID: rows[1].AssetID, Type: string(canonical.AssetSoroban), DecimalsUnresolved: true}}
	s.fillContractDecimals(context.Background(), fresh, src)
	if fresh[0].DecimalsUnresolved || !fresh[0].DecimalsUndeclared {
		t.Errorf("after a successful empty read: unresolved=%v undeclared=%v, want undeclared only", fresh[0].DecimalsUnresolved, fresh[0].DecimalsUndeclared)
	}
}

// sorobanListingReader serves one unpriced Soroban row through the listing handler.
type sorobanListingReader struct {
	AssetsReader
	contractID string
}

func (r *sorobanListingReader) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return []timescale.AssetRow{{AssetID: r.contractID, Code: "TKN"}}, nil
}

// A listing whose scale read ran out of budget is answered with the scale absent, flagged stale, and never replayed.
func TestAssetList_ScaleBudgetExhaustedIsFlaggedAndNotCached(t *testing.T) {
	stub := &countingScaleReader{batchErr: context.DeadlineExceeded}
	srv := New(Options{Directory: &countingDirectory{}, TokenDecimals: stub})
	srv.AssetsReader = &sorobanListingReader{contractID: syntheticContractID(t, 1)}

	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets?limit=100", nil))
		if rec.Code != http.StatusOK || rec.Header().Get("X-Stellarindex-Cache") == "HIT" {
			t.Fatalf("request %d: status=%d cache=%q, want an uncached 200", i, rec.Code, rec.Header().Get("X-Stellarindex-Cache"))
		}
		var env struct {
			Data  []map[string]any `json:"data"`
			Flags Flags            `json:"flags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if len(env.Data) != 1 {
			t.Fatalf("instrument: want one row, got %s", rec.Body.String())
		}
		if _, has := env.Data[0]["decimals"]; has {
			t.Errorf("request %d: unread scale served as decimals=%v", i, env.Data[0]["decimals"])
		}
		if !env.Flags.Stale {
			t.Errorf("request %d: flags.stale=false on a page whose scale read was cut short", i)
		}
	}
	if got := stub.batchN.Load(); got != 2 {
		t.Errorf("scale read %d times over two requests, want 2 (the cut-short page was replayed)", got)
	}
}
