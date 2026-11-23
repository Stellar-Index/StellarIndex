// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// declaredPegOnlyServer prices a declared USD peg with no market anywhere, so the only USD price
// is the operator's 1:1 declaration.
func declaredPegOnlyServer(t *testing.T) (*Server, canonical.Asset) {
	t.Helper()
	peg := mustAsset(t, withheldTestPegIssuer)
	return &Server{Options: Options{
		Prices:            withheldUSDReader{},
		USDPeggedClassics: []canonical.Asset{peg},
		Change24h:         stubChange24h{then: "0.90"},
	}}, peg
}

type pegAssetReader struct{ d AssetDetail }

func (r pegAssetReader) GetAsset(context.Context, canonical.Asset) (AssetDetail, error) {
	return r.d, nil
}

func (pegAssetReader) ListAssets(context.Context, string, int) ([]AssetDetail, string, error) {
	return nil, "", nil
}

// TestAssetDetail_DeclarationValuesNoCap: GET /v1/assets/{id} serves the declaration labelled declared_peg, filled last, so no
// market cap or FDV is built on it.
func TestAssetDetail_DeclarationValuesNoCap(t *testing.T) {
	_, peg := declaredPegOnlyServer(t)
	srv := New(Options{
		Assets:            pegAssetReader{d: AssetDetail{AssetID: peg.String(), Type: "classic", Code: peg.Code, Decimals: 7}},
		Prices:            withheldUSDReader{},
		USDPeggedClassics: []canonical.Asset{peg},
		Supply: capSupplyStub{snap: supply.Supply{
			CirculatingSupply: big.NewInt(50_000_000_000_000_000), MaxSupply: big.NewInt(90_000_000_000_000_000), ObservedAt: time.Now(),
		}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/assets/" + peg.String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	body := string(b)
	if !strings.Contains(body, `"price_usd":"`+declaredPegPrice+`"`) || !strings.Contains(body, `"price_basis":"declared_peg"`) {
		t.Fatalf("want the labelled declaration served: %s", body)
	}
	if strings.Contains(body, `"market_cap_usd":"`) || strings.Contains(body, `"fdv_usd":"`) {
		t.Errorf("market cap or FDV built on the declared peg: %s", body)
	}
}

// TestPopulateChange24h_DeclarationHasNoChange: a declared 1.0 compared with a market bucket
// would report a move nobody observed.
func TestPopulateChange24h_DeclarationHasNoChange(t *testing.T) {
	s, peg := declaredPegOnlyServer(t)
	var detail AssetDetail
	if failed := s.populateChange24h(context.Background(), &detail, peg); failed {
		t.Fatal("change read reported failed")
	}
	if detail.Change24hPct != nil {
		t.Fatalf("change_24h_pct = %q on a declared price, want absent", *detail.Change24hPct)
	}
}

// TestReservePriceUSD_TakesNoDeclaration: the lending page values reserves on observed prices
// only, so a declared peg with no market leaves the reserve unpriced.
func TestReservePriceUSD_TakesNoDeclaration(t *testing.T) {
	s, peg := declaredPegOnlyServer(t)
	if got := s.reservePriceUSD(context.Background(), peg.String()); got.price != "" {
		t.Fatalf("reserve price = %q (basis %q), want none from a declaration", got.price, got.basis)
	}
}
