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
	"github.com/Stellar-Index/StellarIndex/internal/currency"
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

// TestAssetDetail_GlobalMarketFillsAheadOfTheUSDDeclaration: a USD-peg declaration with no market price and a fresh vetted global
// reference serves the reference as global_market, so a global depeg reaches price_usd; without it, the bare declaration. Neither
// builds a cap, FDV or change on the whole-token price.
func TestAssetDetail_GlobalMarketFillsAheadOfTheUSDDeclaration(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	peg := mustAsset(t, globalTestUSDC)
	for name, tc := range map[string]struct {
		streams []canonical.OracleUpdate
		want    []string
		absent  []string
	}{
		"fresh global reference": {
			streams: []canonical.OracleUpdate{refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now().Add(-5*time.Minute))},
			want:    []string{`"price_usd":"0.97`, `"price_basis":"global_market"`, `"global_market":{`},
			absent:  []string{`"price_basis":"declared_peg"`, `"price_usd":"` + declaredPegPrice + `"`},
		},
		"no reference": {
			want:   []string{`"price_usd":"` + declaredPegPrice + `"`, `"price_basis":"declared_peg"`},
			absent: []string{`"global_market":{`, `"change_24h_pct":"`, `"change_1h_pct":"`, `"change_7d_pct":"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := New(Options{
				Assets:             pegAssetReader{d: AssetDetail{AssetID: peg.String(), Type: "classic", Code: peg.Code, Decimals: 7}},
				Prices:             withheldUSDReader{},
				USDPeggedClassics:  []canonical.Asset{peg},
				VerifiedCurrencies: cat,
				Oracle:             &streamOracle{streams: tc.streams},
				Change24h:          stubChange24h{then: "0.90"},
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
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %s: %s", w, body)
				}
			}
			for _, a := range append(tc.absent, `"market_cap_usd":"`, `"fdv_usd":"`) {
				if strings.Contains(body, a) {
					t.Errorf("served %s on a whole-token fill: %s", a, body)
				}
			}
		})
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
