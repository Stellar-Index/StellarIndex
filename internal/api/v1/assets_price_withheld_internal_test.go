// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	withheldTestSecondPeg = "USDA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestPopulatePriceUSD_WithheldIsNotAbsent pins the price producer's three outcomes: a withheld direct read must neither fall
// through to the stablecoin proxy (a side door around the refusal) nor read as "never traded"; the row carries the withheld
// reason.
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
			name: "no direct market, own market via the peg withheld",
			reader: withheldUSDReader{
				viaPeg: {err: newPriceWithheld(PriceWithheldSubstance)},
			},
			wantReason: PriceWithheldSubstance,
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

// TestPopulatePriceUSD_DeclaredPegWithheldReason: a declared peg is priced through its XLM book,
// which is its OWN market, so a refusal there reads as the listing does (`substance` below the
// floor, `unattributed` when the read stated nothing). Only the XLM/USD pivot behind it is a leg
// beyond the asset's market, `upstream_leg`.
func TestPopulatePriceUSD_DeclaredPegWithheldReason(t *testing.T) {
	peg := mustAsset(t, withheldTestPegIssuer)
	ownBook := withheldTestPegIssuer + "/" + canonical.NativeAsset().String()
	pivot := canonical.NativeAsset().String() + "/" + defaultPriceQuote.String()
	cases := []struct {
		name   string
		reader withheldUSDReader
		want   PriceWithheldReason
	}{
		{
			name:   "own XLM book below the floor",
			reader: withheldUSDReader{ownBook: {err: newPriceWithheld(PriceWithheldSubstance)}},
			want:   PriceWithheldSubstance,
		},
		{
			name:   "own XLM book withheld without a reason",
			reader: withheldUSDReader{ownBook: {err: ErrPriceWithheld}},
			want:   PriceWithheldUnattributed,
		},
		{
			name:   "XLM/USD pivot below the floor",
			reader: withheldUSDReader{ownBook: {price: "6.25"}, pivot: {err: newPriceWithheld(PriceWithheldSubstance)}},
			want:   PriceWithheldUpstreamLeg,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{Prices: tc.reader, USDPeggedClassics: []canonical.Asset{peg}}}
			var detail AssetDetail
			s.populatePriceUSD(context.Background(), &detail, peg)
			if detail.PriceUSD != nil || detail.PriceWithheldReason != tc.want {
				t.Fatalf("price_usd=%v price_withheld_reason=%q, want nil and %q", detail.PriceUSD, detail.PriceWithheldReason, tc.want)
			}
		})
	}
}

// TestWalkUSDPegs_WithheldReasonRank: when no peg serves, the walk reports the loudest reason
// whatever the peg order: the asset's own market below the floor over a withheld peg leg over a
// refusal that stated nothing.
func TestWalkUSDPegs_WithheldReasonRank(t *testing.T) {
	asset := mustAsset(t, withheldTestAsset)
	pegA := mustAsset(t, withheldTestPegIssuer)
	pegB := mustAsset(t, withheldTestSecondPeg)
	viaA := asset.String() + "/" + pegA.String()
	viaB := asset.String() + "/" + pegB.String()
	flagged := newPriceWithheld(PriceWithheldScamIssuer)
	thin := newPriceWithheld(PriceWithheldSubstance)
	cases := []struct {
		name   string
		reader withheldUSDReader
		want   PriceWithheldReason
	}{
		{"flagged then thin", withheldUSDReader{viaA: {err: flagged}, viaB: {err: thin}}, PriceWithheldSubstance},
		{"thin then flagged", withheldUSDReader{viaA: {err: thin}, viaB: {err: flagged}}, PriceWithheldSubstance},
		{"flagged peg only", withheldUSDReader{viaA: {err: flagged}}, PriceWithheldUpstreamLeg},
		{"no stated reason", withheldUSDReader{viaA: {err: ErrPriceWithheld}}, PriceWithheldUnattributed},
		{"unstated then flagged", withheldUSDReader{viaA: {err: ErrPriceWithheld}, viaB: {err: flagged}}, PriceWithheldUpstreamLeg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{Prices: tc.reader, USDPeggedClassics: []canonical.Asset{pegA, pegB}}}
			_, _, ok, got := s.walkUSDPegs(context.Background(), asset, defaultPriceQuote)
			if ok || got != tc.want {
				t.Fatalf("ok=%v reason=%q, want false and %q", ok, got, tc.want)
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
	credited := AssetDetail{
		PriceUSD: &price, PriceAuthority: "aggregator_avg", PriceSources: []string{"coingecko"},
		PriceAsOf: new(WireTime), PriceCredit: "CoinGecko", issuerDirectoryUnchecked: true,
	}
	suppressScamIssuerPricing(&credited)
	if credited.PriceWithheldReason != PriceWithheldUnattributed || credited.PriceAuthority != "" || credited.PriceSources != nil || credited.PriceAsOf != nil || credited.PriceCredit != "" {
		t.Fatalf("withheld row kept its provenance: reason=%q authority=%q sources=%v as_of=%v credit=%q", credited.PriceWithheldReason, credited.PriceAuthority, credited.PriceSources, credited.PriceAsOf, credited.PriceCredit)
	}
	unpriced := AssetDetail{IssuerDirectoryTags: []string{"scam"}}
	suppressScamIssuerPricing(&unpriced)
	if unpriced.PriceWithheldReason != "" {
		t.Fatalf("never-priced flagged asset stamped %q: nothing was withheld", unpriced.PriceWithheldReason)
	}
	// The asset's own flag outranks whatever the reader-side walk attributed to a peg leg.
	walked := AssetDetail{PriceWithheldReason: PriceWithheldUpstreamLeg, IssuerDirectoryTags: []string{"scam"}}
	suppressScamIssuerPricing(&walked)
	if walked.PriceWithheldReason != PriceWithheldScamIssuer {
		t.Fatalf("flagged issuer behind a walked reason stamped %q, want %q", walked.PriceWithheldReason, PriceWithheldScamIssuer)
	}
	// An unanswered directory lookup never overwrites a reason the walk already named.
	unchecked := AssetDetail{PriceWithheldReason: PriceWithheldSubstance, issuerDirectoryUnchecked: true}
	suppressScamIssuerPricing(&unchecked)
	if unchecked.PriceWithheldReason != PriceWithheldSubstance {
		t.Fatalf("unchecked lookup overwrote %q with %q", PriceWithheldSubstance, unchecked.PriceWithheldReason)
	}
}

// The withheld 404 must carry the reason as a member, so clients branch
// on it rather than string-matching the title.
func TestWritePriceWithheldProblem_CarriesReasonMember(t *testing.T) {
	asset := canonical.NativeAsset()
	quote := defaultPriceQuote
	cases := map[PriceWithheldReason]string{
		PriceWithheldSubstance:         "substance",
		PriceWithheldScamIssuer:        "scam_issuer",
		PriceWithheldUpstreamLeg:       "upstream_leg",
		PriceWithheldUnattributed:      "unattributed",
		PriceWithheldManipulationGuard: "manipulation_guard",
		PriceWithheldFXLeg:             "fx_leg_unavailable",
		"":                             "unattributed",
	}
	for in, want := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil)
		writePriceWithheldProblem(rec, req, asset, quote, in)

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("reason %q: decode: %v", in, err)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("reason %q: status %d, want 404", in, rec.Code)
		}
		if got := body["reason"]; got != want {
			t.Errorf("reason %q: body reason = %v, want %q", in, got, want)
		}
	}
}
