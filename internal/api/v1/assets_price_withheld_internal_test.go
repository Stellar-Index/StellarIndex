// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// withheldUSDReader answers LatestPrice per pair from a fixed table; a pair
// not in the table is ErrPriceNotFound.
type withheldUSDReader map[string]struct {
	price string
	err   error
}

func (r withheldUSDReader) LatestPrice(_ context.Context, asset, quote canonical.Asset) (PriceSnapshot, []string, bool, error) {
	row, ok := r[asset.String()+"/"+quote.String()]
	if !ok {
		return PriceSnapshot{}, nil, false, ErrPriceNotFound
	}
	if row.err != nil {
		return PriceSnapshot{}, nil, false, row.err
	}
	return PriceSnapshot{AssetID: asset.String(), Quote: quote.String(), Price: row.price}, []string{"sdex"}, false, nil
}

func (withheldUSDReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, ErrPriceNotFound
}

const (
	withheldTestAsset     = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	withheldTestPegIssuer = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestPopulatePriceUSD_WithheldIsNotAbsent pins the /v1/assets/{id} price
// producer's three outcomes. A withheld direct read must neither fall
// through to the stablecoin proxy (which re-serves the refused market via
// a side door, as /v1/price refuses to) nor read as "never traded": the
// row carries the price-withheld reason instead of a bare null.
func TestPopulatePriceUSD_WithheldIsNotAbsent(t *testing.T) {
	asset := mustAsset(t, withheldTestAsset)
	peg := mustAsset(t, withheldTestPegIssuer)
	direct := asset.String() + "/" + defaultPriceQuote.String()
	viaPeg := asset.String() + "/" + peg.String()

	cases := []struct {
		name       string
		reader     withheldUSDReader
		wantPrice  string
		wantReason PriceWithheldReason
	}{
		{
			name: "direct read withheld, proxy market would serve",
			reader: withheldUSDReader{
				direct: {err: newPriceWithheld(PriceWithheldSubstance)},
				viaPeg: {price: "0.5"},
			},
			wantReason: PriceWithheldSubstance,
		},
		{
			name: "direct read withheld as scam issuer",
			reader: withheldUSDReader{
				direct: {err: newPriceWithheld(PriceWithheldScamIssuer)},
			},
			wantReason: PriceWithheldScamIssuer,
		},
		{
			name: "no direct market, proxy peg leg withheld",
			reader: withheldUSDReader{
				viaPeg: {err: newPriceWithheld(PriceWithheldSubstance)},
			},
			wantReason: PriceWithheldUpstreamLeg,
		},
		{
			name:   "never traded",
			reader: withheldUSDReader{},
		},
		{
			name:      "served directly",
			reader:    withheldUSDReader{direct: {price: "0.25"}},
			wantPrice: "0.25",
		},
		{
			name:      "served via proxy",
			reader:    withheldUSDReader{viaPeg: {price: "0.5"}},
			wantPrice: "0.5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{Prices: tc.reader, USDPeggedClassics: []canonical.Asset{peg}}}
			var detail AssetDetail
			sources := s.populatePriceUSD(context.Background(), &detail, asset)

			gotPrice := ""
			if detail.PriceUSD != nil {
				gotPrice = *detail.PriceUSD
			}
			if gotPrice != tc.wantPrice {
				t.Errorf("price_usd = %q, want %q", gotPrice, tc.wantPrice)
			}
			if detail.PriceWithheldReason != tc.wantReason {
				t.Errorf("price_withheld_reason = %q, want %q", detail.PriceWithheldReason, tc.wantReason)
			}
			if (tc.wantPrice == "") != (sources == 0) {
				t.Errorf("source count = %d for price %q", sources, tc.wantPrice)
			}

			body, err := json.Marshal(detail)
			if err != nil {
				t.Fatal(err)
			}
			wire := `"price_withheld_reason":"` + string(tc.wantReason) + `"`
			if got := strings.Contains(string(body), wire); got != (tc.wantReason != "") {
				t.Errorf("wire body %s: contains %s = %v", body, wire, got)
			}
		})
	}
}

// TestDeclaredPegFillClearsWithheldReason: the reason describes a null
// price_usd, so a producer that later fills the price must drop it.
func TestDeclaredPegFillClearsWithheldReason(t *testing.T) {
	row := AssetDetail{AssetID: withheldTestAsset, PriceWithheldReason: PriceWithheldSubstance}
	price := "1.0"
	s := &Server{Options: Options{FiatPeggedClassics: map[string]canonical.Asset{withheldTestAsset: mustAsset(t, "fiat:AUD")}}}
	s.fillDeclaredPegPrice(context.Background(), &row, map[string]*string{"AUD": &price})
	if row.PriceUSD == nil || row.PriceWithheldReason != "" {
		t.Fatalf("after peg fill: price_usd=%v price_withheld_reason=%q, want price and no reason", row.PriceUSD, row.PriceWithheldReason)
	}
}

// TestScamSuppressionStampsWithheldReason: nulling a served price for a
// flagged issuer is a withholding, and the row says so.
func TestScamSuppressionStampsWithheldReason(t *testing.T) {
	price := "2"
	flagged := AssetDetail{PriceUSD: &price, IssuerDirectoryTags: []string{"scam"}}
	suppressScamIssuerPricing(&flagged)
	if flagged.PriceUSD != nil || flagged.PriceWithheldReason != PriceWithheldScamIssuer {
		t.Fatalf("flagged: price_usd=%v reason=%q, want nil and %q", flagged.PriceUSD, flagged.PriceWithheldReason, PriceWithheldScamIssuer)
	}
	unpriced := AssetDetail{IssuerDirectoryTags: []string{"scam"}}
	suppressScamIssuerPricing(&unpriced)
	if unpriced.PriceWithheldReason != "" {
		t.Fatalf("never-priced flagged asset stamped %q: nothing was withheld", unpriced.PriceWithheldReason)
	}
}
