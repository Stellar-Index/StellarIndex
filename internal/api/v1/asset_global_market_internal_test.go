package v1

import (
	"context"
	"log/slog"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

const (
	globalTestUSDC          = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	globalTestUSDCLookalike = "USDC-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
)

// streamOracle answers LatestOracleStreams with a fixed row set.
type streamOracle struct{ streams []canonical.OracleUpdate }

func (o *streamOracle) LatestOracleUpdatesForAsset(context.Context, canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (o *streamOracle) LatestOracleUpdatesForAssets(context.Context, []canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (o *streamOracle) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	return o.streams, nil
}

// globalMarketServer has USDC-GA5Z… vetted by the embedded catalogue AND
// declared a 1:1 USD peg, so a fixed-peg fill would print exactly $1.
func globalMarketServer(t *testing.T, streams ...canonical.OracleUpdate) *Server {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		logger:             slog.Default(),
		verifiedCurrencies: cat,
		oracle:             &streamOracle{streams: streams},
		fiatPeggedClassics: map[string]canonical.Asset{globalTestUSDC: usd, globalTestUSDCLookalike: usd},
	}
}

// A vetted same-asset token with no surviving Stellar price is priced from
// its global market, not the fixed peg: a global depeg must reach price_usd.
func TestGlobalMarket_FillsAheadOfFixedPeg(t *testing.T) {
	s := globalMarketServer(t,
		refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now().Add(-5*time.Minute)))
	rows := []AssetDetail{{AssetID: globalTestUSDC, Code: "USDC"}}
	s.fillDeclaredPegPricesInListing(context.Background(), rows)

	if got := strPtr(rows[0].PriceUSD); got != "0.97000000" {
		t.Fatalf("price_usd = %s, want 0.97000000 (the global market, not the 1:1 peg)", got)
	}
	if rows[0].PriceBasis != priceBasisGlobalMarket {
		t.Errorf("price_basis = %q, want %q", rows[0].PriceBasis, priceBasisGlobalMarket)
	}
	gm := rows[0].GlobalMarket
	if gm == nil || gm.Asset != "crypto:USDC" || gm.Source != "coingecko" || gm.StellarDivergencePct != nil {
		t.Fatalf("global_market = %+v, want crypto:USDC from coingecko with no Stellar divergence", gm)
	}
	if priceSeriesPublishable(&rows[0]) {
		t.Error("a global-market-filled row must not publish its Stellar price series")
	}
}

// A lookalike sharing the ticker never gets the vetted asset's global
// price; it falls through to whatever its own declaration says.
func TestGlobalMarket_LookalikeNeverTrusted(t *testing.T) {
	s := globalMarketServer(t,
		refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()))
	rows := []AssetDetail{{AssetID: globalTestUSDCLookalike, Code: "USDC"}}
	s.fillDeclaredPegPricesInListing(context.Background(), rows)

	if rows[0].GlobalMarket != nil || rows[0].PriceBasis == priceBasisGlobalMarket {
		t.Fatalf("lookalike got the vetted global price: basis=%q global_market=%+v", rows[0].PriceBasis, rows[0].GlobalMarket)
	}
}

// A surviving Stellar price is kept and measured against the global one.
func TestGlobalMarket_StellarDivergenceWarning(t *testing.T) {
	cases := []struct {
		stellar, wantPct string
		wantWarn         bool
	}{
		{"0.90", "-10.00", true},
		{"0.999", "-0.10", false},
		{"1.0600001", "+6.00", true},
	}
	for _, tc := range cases {
		s := globalMarketServer(t,
			refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "100000000", 8, time.Now()))
		p := tc.stellar
		rows := []AssetDetail{{AssetID: globalTestUSDC, PriceUSD: &p}}
		s.fillDeclaredPegPricesInListing(context.Background(), rows)

		if strPtr(rows[0].PriceUSD) != tc.stellar || rows[0].PriceBasis != "" {
			t.Fatalf("%s: Stellar market price replaced: %s basis=%q", tc.stellar, strPtr(rows[0].PriceUSD), rows[0].PriceBasis)
		}
		gm := rows[0].GlobalMarket
		if gm == nil || strPtr(gm.StellarDivergencePct) != tc.wantPct || gm.DepegWarning != tc.wantWarn {
			t.Errorf("%s: global_market = %+v, want divergence %s warning %v", tc.stellar, gm, tc.wantPct, tc.wantWarn)
		}
	}
}

// Stale or non-aggregator rows are not a global market.
func TestGlobalMarket_RefusesStaleAndNonAggregatorRows(t *testing.T) {
	for name, u := range map[string]canonical.OracleUpdate{
		"stale":     refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now().Add(-2*time.Hour)),
		"oracle":    refUpdate(t, "redstone", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()),
		"non-usd":   refUpdate(t, "coingecko", "crypto:USDC", "fiat:EUR", "97000000", 8, time.Now()),
		"other-tkr": refUpdate(t, "coingecko", "crypto:USDT", "fiat:USD", "97000000", 8, time.Now()),
	} {
		s := globalMarketServer(t, u)
		rows := []AssetDetail{{AssetID: globalTestUSDC}}
		s.fillDeclaredPegPricesInListing(context.Background(), rows)
		if rows[0].GlobalMarket != nil || rows[0].PriceBasis != priceBasisDeclaredPeg {
			t.Errorf("%s: basis=%q global_market=%+v, want the declared-peg fallback", name, rows[0].PriceBasis, rows[0].GlobalMarket)
		}
	}
}

// The detail path takes the same route, and a scam-class suppression
// withholds the global price with every other dollar figure.
func TestGlobalMarket_DetailPathAndScamSuppression(t *testing.T) {
	s := globalMarketServer(t,
		refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()))
	d := AssetDetail{AssetID: globalTestUSDC}
	s.fillDeclaredPegPrice(context.Background(), &d, nil)
	if d.PriceBasis != priceBasisGlobalMarket || d.GlobalMarket == nil {
		t.Fatalf("detail: basis=%q global_market=%+v, want a global-market fill", d.PriceBasis, d.GlobalMarket)
	}
	d.issuerDirectoryUnchecked = true
	suppressScamIssuerPricing(&d)
	if d.PriceUSD != nil || d.GlobalMarket != nil {
		t.Errorf("suppression left price_usd=%s global_market=%+v", strPtr(d.PriceUSD), d.GlobalMarket)
	}
}

// A future-dated aggregator row neither serves nor displaces a valid one.
func TestGlobalMarket_RefusesFutureDatedRows(t *testing.T) {
	future := refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "50000000", 8, time.Now().Add(24*time.Hour))
	s := globalMarketServer(t, future)
	rows := []AssetDetail{{AssetID: globalTestUSDC}}
	s.fillDeclaredPegPricesInListing(context.Background(), rows)
	if rows[0].GlobalMarket != nil || rows[0].PriceBasis != priceBasisDeclaredPeg {
		t.Fatalf("future row served: basis=%q global_market=%+v", rows[0].PriceBasis, rows[0].GlobalMarket)
	}

	s = globalMarketServer(t, future,
		refUpdate(t, "coinmarketcap", "crypto:USDC", "fiat:USD", "99000000", 8, time.Now().Add(-time.Minute)))
	rows = []AssetDetail{{AssetID: globalTestUSDC}}
	s.fillDeclaredPegPricesInListing(context.Background(), rows)
	if gm := rows[0].GlobalMarket; gm == nil || gm.PriceUSD != "0.99000000" {
		t.Fatalf("global_market = %+v, want the valid 0.99000000 row, not the future one", gm)
	}

	refs := map[string]rwaReference{"crypto:USDC": {
		priceUSD: big.NewRat(1, 1), wire: "1", source: "coingecko", asOf: time.Now().Add(time.Hour),
	}}
	d := AssetDetail{AssetID: globalTestUSDC}
	s.applyGlobalMarket(&d, refs, time.Now())
	if d.GlobalMarket != nil || d.PriceUSD != nil {
		t.Errorf("a cached future-dated reference was applied: %+v", d.GlobalMarket)
	}
}

// A declared USD peg is the USD proxy: its Stellar price is quoted through
// itself, so no divergence or warning is stamped on it.
func TestGlobalMarket_DeclaredUSDPegCarriesNoDivergence(t *testing.T) {
	s := globalMarketServer(t,
		refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "90000000", 8, time.Now()))
	usdc, err := canonical.ParseAsset(globalTestUSDC)
	if err != nil {
		t.Fatal(err)
	}
	s.usdPeggedClassics = []canonical.Asset{usdc}
	p := "1.00"
	rows := []AssetDetail{{AssetID: globalTestUSDC, PriceUSD: &p}}
	s.fillDeclaredPegPricesInListing(context.Background(), rows)
	gm := rows[0].GlobalMarket
	if gm == nil || gm.StellarDivergencePct != nil || gm.DepegWarning {
		t.Fatalf("global_market = %+v, want the reference without a divergence", gm)
	}
}

// A depeg warning carries the issuer behaviours that let the issuer move
// balances; a quiet market carries none.
func TestGlobalMarket_DepegWarningFoldsIssuerSignals(t *testing.T) {
	yes, no, claw := true, false, "250"
	for _, tc := range []struct {
		stellar string
		want    []string
	}{
		{"0.90", []string{issuerSignalClawbackEnabled, issuerSignalClawbackObserved}},
		{"0.999", nil},
	} {
		s := globalMarketServer(t,
			refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "100000000", 8, time.Now()))
		p := tc.stellar
		d := AssetDetail{AssetID: globalTestUSDC, PriceUSD: &p, IssuerBehaviour: &AssetIssuerBehaviour{
			AuthClawbackEnabled: &yes, AuthRevocable: &no, ClawbackTotal: &claw,
		}}
		s.fillDeclaredPegPrice(context.Background(), &d, nil)
		if d.GlobalMarket == nil || !reflect.DeepEqual(d.GlobalMarket.IssuerSignals, tc.want) {
			t.Errorf("%s: global_market = %+v, want issuer_signals %v", tc.stellar, d.GlobalMarket, tc.want)
		}
	}
}
