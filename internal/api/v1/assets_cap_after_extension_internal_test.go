// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

type capSupplyStub struct{ snap supply.Supply }

func (s capSupplyStub) LatestSupply(context.Context, string) (supply.Supply, error) {
	return s.snap, nil
}

func (capSupplyStub) DailyCirculatingSupply(context.Context, string, time.Time, time.Time) ([]timescale.SupplyDayPoint, error) {
	return nil, nil
}

// TestMarketCap_PriceFilledAfterF2IsValuedUnderTheSameGuards — a price only the extension step finds (transitive here) gets
// the market cap F2 could not compute, and the dust floor and turnover ceiling still refuse it.
func TestMarketCap_PriceFilledAfterF2IsValuedUnderTheSameGuards(t *testing.T) {
	const (
		assetID = "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
		hopID   = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	)
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	vol := func(v string) *string { return &v }
	for _, tc := range []struct {
		name      string
		volume    *string
		floor     float64
		wantCap   string
		wantLowLq bool
	}{
		{"no turnover reading", nil, 0, "767000.00", false},
		{"cap within the turnover ceiling", vol("10000"), 1000, "767000.00", false},
		{"cap above the turnover ceiling", vol("100"), 0, "", true},
		{"single-venue sub-floor volume", vol("100"), 1000, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				Options: Options{
					Supply: capSupplyStub{snap: supply.Supply{CirculatingSupply: big.NewInt(100_000_000_000_000), ObservedAt: time.Now()}},
					TransitivePricer: &stubPricer{ok: true, tp: timescale.TransitivePrice{
						PriceUSD: "0.0767", Hop: hopID, HopVolume24hUSD: "5000", HopQuote: "native",
					}},
					Substance:               &stubListingGate{allow: map[string]bool{assetID + "|" + hopID: true, hopID + "|native": true}},
					MaxMarketCapVolumeRatio: 100,
					MinMarketCapVolumeUSD:   tc.floor,
				},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			detail := AssetDetail{Decimals: 7, VolumeUSD24h: tc.volume}
			s.applyF2Fields(context.Background(), &detail, asset)
			s.applyAssetRowToDetail(context.Background(), &detail, asset, timescale.AssetRow{}, sql.ErrNoRows, assetID)
			if detail.PriceUSD == nil || detail.PriceBasis != priceBasisTransitive {
				t.Fatalf("price = %v basis = %q, want the transitive fill", detail.PriceUSD, detail.PriceBasis)
			}
			if got := strOrEmpty(detail.MarketCapUSD); got != tc.wantCap || detail.MarketCapLowLiquidity != tc.wantLowLq {
				t.Errorf("market_cap_usd = %q low_liquidity = %v, want %q %v", got, detail.MarketCapLowLiquidity, tc.wantCap, tc.wantLowLq)
			}
		})
	}
}

const (
	capTestAsset = "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	capTestHop   = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	// capTestImpostor claims the catalogue's BLND ticker under an issuer the catalogue does not list.
	capTestImpostor = "BLND-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
)

// capTestServer prices capTestAsset at 0.0767 through the transitive step, over 10^7 circulating and 2x10^7 max tokens.
func capTestServer(prices PriceReader, snap supply.Supply) *Server {
	return &Server{
		Options: Options{
			Prices: prices,
			Supply: capSupplyStub{snap: snap},
			TransitivePricer: &stubPricer{ok: true, tp: timescale.TransitivePrice{
				PriceUSD: "0.0767", Hop: capTestHop, HopVolume24hUSD: "5000", HopQuote: "native",
			}},
			Substance: &stubListingGate{allow: map[string]bool{
				capTestAsset + "|native": true, capTestAsset + "|" + capTestHop: true, capTestHop + "|native": true,
				capTestImpostor + "|native": true, capTestImpostor + "|" + capTestHop: true,
			}},
			MaxMarketCapVolumeRatio: 100,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func capTestSupply() supply.Supply {
	return supply.Supply{CirculatingSupply: big.NewInt(100_000_000_000_000), MaxSupply: big.NewInt(200_000_000_000_000), ObservedAt: time.Now()}
}

// capTestRun runs F2 then the extension row step: withRow fills from a catalogue row price, otherwise there is no row and the
// transitive step fills.
func capTestRun(t *testing.T, s *Server, withRow bool) AssetDetail {
	t.Helper()
	return capTestRunAsset(t, s, capTestAsset, withRow, nil)
}

// capTestRunAsset is capTestRun for assetID; between the two steps it applies between to the detail.
func capTestRunAsset(t *testing.T, s *Server, assetID string, withRow bool, between func(*AssetDetail)) AssetDetail {
	t.Helper()
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	detail := AssetDetail{Decimals: 7}
	s.applyF2Fields(context.Background(), &detail, asset)
	if between != nil {
		between(&detail)
	}
	row, rowErr := timescale.AssetRow{}, error(sql.ErrNoRows)
	if withRow {
		p := "0.0767"
		row, rowErr = timescale.AssetRow{PriceUSD: &p}, nil
	}
	s.applyAssetRowToDetail(context.Background(), &detail, asset, row, rowErr, assetID)
	return detail
}

// TestMarketCap_FilledPriceGuards — the deferred cap over a catalogue-row or transitive fill keeps populateMarketCap's guards:
// a thin price, a stale supply observation and an unverified ticker collision each value no cap or FDV.
func TestMarketCap_FilledPriceGuards(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	stale := capTestSupply()
	stale.ObservedAt = time.Now().Add(-preciseSupplyMaxAge - time.Hour)
	thin := func(d *AssetDetail) {
		p := "0.0767"
		d.PriceUSD, d.ThinMarket = &p, true
	}
	for _, withRow := range []bool{true, false} {
		t.Run(fmt.Sprintf("catalogue_row=%v", withRow), func(t *testing.T) {
			if d := capTestRun(t, capTestServer(withheldUSDReader{}, capTestSupply()), withRow); strOrEmpty(d.MarketCapUSD) != "767000.00" {
				t.Errorf("fill: market_cap_usd = %q, want 767000.00", strOrEmpty(d.MarketCapUSD))
			}
			if d := capTestRunAsset(t, capTestServer(withheldUSDReader{}, capTestSupply()), capTestAsset, withRow, thin); d.MarketCapUSD != nil || d.FDVUSD != nil {
				t.Errorf("thin price: market_cap_usd = %q fdv_usd = %q, want both null", strOrEmpty(d.MarketCapUSD), strOrEmpty(d.FDVUSD))
			}
			if d := capTestRun(t, capTestServer(withheldUSDReader{}, stale), withRow); d.PriceUSD == nil || d.MarketCapUSD != nil || d.FDVUSD != nil || !d.SupplyStale {
				t.Errorf("stale supply: price %v market_cap_usd = %q fdv_usd = %q supply_stale = %v, want a price, no cap, no FDV, stale",
					d.PriceUSD, strOrEmpty(d.MarketCapUSD), strOrEmpty(d.FDVUSD), d.SupplyStale)
			}
			collide := capTestServer(withheldUSDReader{}, capTestSupply())
			collide.VerifiedCurrencies = cat
			if d := capTestRunAsset(t, collide, capTestImpostor, withRow, nil); d.PriceUSD == nil || d.MarketCapUSD != nil || d.FDVUSD != nil {
				t.Errorf("ticker collision: price %v market_cap_usd = %q fdv_usd = %q, want a price and no cap or FDV",
					d.PriceUSD, strOrEmpty(d.MarketCapUSD), strOrEmpty(d.FDVUSD))
			}
			if d := capTestRun(t, collide, withRow); strOrEmpty(d.MarketCapUSD) != "767000.00" {
				t.Errorf("verified issuer under the catalogue: market_cap_usd = %q, want 767000.00", strOrEmpty(d.MarketCapUSD))
			}
		})
	}
}

// TestMarketCap_FilledPriceValuesNoCapWhenF2RefusedOrCouldNotRead — a price the direct reader refused, or could not read, is no
// gap for a later fill to close: neither the catalogue row nor the transitive step may then value a market cap or FDV.
func TestMarketCap_FilledPriceValuesNoCapWhenF2RefusedOrCouldNotRead(t *testing.T) {
	direct := capTestAsset + "/" + defaultPriceQuote.String()
	for _, tc := range []struct {
		name   string
		reader withheldUSDReader
	}{
		{"withheld scam_issuer", withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldScamIssuer)}}},
		{"withheld upstream_leg", withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldUpstreamLeg)}}},
		{"withheld substance", withheldUSDReader{direct: {err: newPriceWithheld(PriceWithheldSubstance)}}},
		{"price read failed", withheldUSDReader{direct: {err: errors.New("price store unreachable")}}},
	} {
		for _, withRow := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/catalogue_row=%v", tc.name, withRow), func(t *testing.T) {
				detail := capTestRun(t, capTestServer(tc.reader, capTestSupply()), withRow)
				if detail.PriceUSD == nil {
					t.Fatal("price_usd = nil, want the later fill (the case under test)")
				}
				if detail.MarketCapUSD != nil || detail.FDVUSD != nil {
					t.Errorf("market_cap_usd = %q fdv_usd = %q, want both null", strOrEmpty(detail.MarketCapUSD), strOrEmpty(detail.FDVUSD))
				}
			})
		}
	}
	// Control: the same fills value the cap when F2 simply found no market.
	for _, withRow := range []bool{true, false} {
		detail := capTestRun(t, capTestServer(withheldUSDReader{}, capTestSupply()), withRow)
		if strOrEmpty(detail.MarketCapUSD) != "767000.00" || strOrEmpty(detail.FDVUSD) != "1534000.00" {
			t.Errorf("catalogue_row=%v: market_cap_usd = %q fdv_usd = %q, want 767000.00 and 1534000.00",
				withRow, strOrEmpty(detail.MarketCapUSD), strOrEmpty(detail.FDVUSD))
		}
	}
}

// TestReadProtocolToken_CarriesPriceBasis — a protocol token priced only through the transitive step names that basis, as the
// asset route does.
func TestReadProtocolToken_CarriesPriceBasis(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	s := capTestServer(withheldUSDReader{}, capTestSupply())
	s.VerifiedCurrencies = cat
	s.AssetsReader = &truthAssetsReader{byID: map[string]timescale.AssetRow{}} // no catalogue row: the transitive step prices
	var flags Flags
	tok, reason := s.readProtocolToken(context.Background(), "BLND", &flags)
	if tok == nil || reason != "" {
		t.Fatalf("token = %+v reason = %q, want a read", tok, reason)
	}
	if strOrEmpty(tok.PriceUSD) != "0.0767" || tok.PriceBasis != priceBasisTransitive {
		t.Errorf("price_usd = %q price_basis = %q, want 0.0767 and %q", strOrEmpty(tok.PriceUSD), tok.PriceBasis, priceBasisTransitive)
	}
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
