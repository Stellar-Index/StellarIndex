package v1_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pegListingRows lists FOO with no market price, so the declared-peg fill prices it.
type pegListingRows struct{ stubAssetsReaderExt }

func (pegListingRows) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return []timescale.AssetRow{{AssetID: exclFOO, Code: "FOO", IssuerGStrkey: strings.SplitN(exclFOO, "-", 2)[1], ObservationCount: 10}}, nil
}

// TestDeclaredPegFX_ExclusionJudgedOnTheWayOut: a classic declared pegged to EUR is priced from the EUR/USD rate. An exclusion
// of the vendor that published it withholds the price with its reason on an uncacheable body; lifting it serves the price on
// the very next request, so no cache holds the judged body; an unreadable set withholds; an empty set changes nothing.
func TestDeclaredPegFX_ExclusionJudgedOnTheWayOut(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), InverseUSDText: "1.17", Source: "ecb"},
	}}
	foo := v1.AssetDetail{AssetID: exclFOO, Type: "classic", Code: "FOO", Decimals: 7, Sep1Status: "not_applicable"}
	opts := func(g v1.PriceExclusionGate) v1.Options {
		return v1.Options{
			Assets:             &stubAssetReader{byID: map[string]v1.AssetDetail{exclFOO: foo}},
			AssetsReader:       &pegListingRows{},
			FXHistory:          fx,
			FiatPeggedClassics: map[string]canonical.Asset{exclFOO: eur},
			Exclusions:         g,
		}
	}
	ecbExcluded := assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "ecb", Base: "fiat:EUR", Quote: "fiat:USD", From: time.Now().Add(-72 * time.Hour),
	}}})

	for _, path := range []string{"/v1/assets", "/v1/assets/" + exclFOO} {
		t.Run(path, func(t *testing.T) {
			_, plain := exclGet(t, v1.New(opts(nil)), path)
			plain = exclAsOf.ReplaceAllString(plain, `"as_of":""`)
			if !strings.Contains(plain, `"price_usd":"1.17"`) || !strings.Contains(plain, `"price_basis":"declared_peg"`) {
				t.Fatalf("no gate: the peg fill did not run: %s", plain)
			}
			gate := &swapGate{g: ecbExcluded}
			ts := startHTTPTest(t, v1.New(opts(gate)).Handler())
			get := func() (string, string) {
				resp := mustGet(t, ts.URL+path)
				body, _ := readAll(resp)
				return exclAsOf.ReplaceAllString(body, `"as_of":""`), resp.Header.Get("Cache-Control")
			}

			body, cc := get()
			if strings.Contains(body, `"price_usd":"`) || !strings.Contains(body, `"price_withheld_reason":"source_excluded"`) ||
				!strings.Contains(body, `"fills_withheld_reason":"source_excluded"`) || cc != "no-store" {
				t.Errorf("ecb excluded: Cache-Control %q, body %s; want price withheld as source_excluded, no-store", cc, body)
			}

			gate.set(assetExclGate(exclTable{}))
			if body, _ = get(); body != plain {
				t.Errorf("exclusion lifted: the peg price is not served again:\n got %s\nwant %s", body, plain)
			}

			gate.set(assetExclGate(exclTable{err: errors.New("db down")}))
			body, cc = get()
			if strings.Contains(body, `"price_usd":"`) || !strings.Contains(body, `"price_withheld_reason":"unattributed"`) ||
				!strings.Contains(body, `"stale":true`) || cc != "no-store" {
				t.Errorf("exclusions unread: Cache-Control %q, body %s; want withheld as unattributed, stale, no-store", cc, body)
			}
		})
	}
}

// pegRoutes is every route that serves a declared-peg price, each wired so the peg is the only price its EUR-pegged member has.
// A new route that serves the fill joins this table.
func pegRoutes(t *testing.T) map[string]func(v1.Options) (string, v1.Options) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	eurc, cetes := "EURC-"+scEURC, "CETES-"+rwaGoodIssuer
	foo := v1.AssetDetail{AssetID: exclFOO, Type: "classic", Code: "FOO", Decimals: 7, Sep1Status: "not_applicable"}
	return map[string]func(v1.Options) (string, v1.Options){
		"assets listing": func(o v1.Options) (string, v1.Options) {
			o.AssetsReader, o.FiatPeggedClassics = &pegListingRows{}, map[string]canonical.Asset{exclFOO: eur}
			return "/v1/assets", o
		},
		"asset detail": func(o v1.Options) (string, v1.Options) {
			o.Assets = &stubAssetReader{byID: map[string]v1.AssetDetail{exclFOO: foo}}
			o.AssetsReader, o.FiatPeggedClassics = &pegListingRows{}, map[string]canonical.Asset{exclFOO: eur}
			return "/v1/assets/" + exclFOO, o
		},
		"stablecoins": func(o v1.Options) (string, v1.Options) {
			o.VerifiedCurrencies, o.FiatPeggedClassics = cat, map[string]canonical.Asset{eurc: eur}
			o.AssetsReader = &rwaListStub{
				stubAssetsReaderExt: &stubAssetsReaderExt{},
				byIssuer:            map[string][]timescale.AssetRow{scEURC: {rwaRow("EURC", scEURC, nil, 9000)}},
				supply:              map[string]string{eurc: "1000000000"},
			}
			return "/v1/stablecoins", o
		},
		"rwa classic arm": func(o v1.Options) (string, v1.Options) {
			o.Listings, o.FiatPeggedClassics = rwaNothingListed, map[string]canonical.Asset{cetes: eur}
			o.Sep1Cache = &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("CETES", rwaGoodIssuer, "etherfuse.com", "bond")}}
			o.Directory = &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}}
			o.AssetsReader = &rwaListStub{
				stubAssetsReaderExt: &stubAssetsReaderExt{},
				byIssuer:            map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("CETES", rwaGoodIssuer, nil, 412009)}},
				supply:              map[string]string{cetes: rwaCETESSupply},
			}
			return "/v1/rwa/assets", o
		},
		"rwa contract arm": func(o v1.Options) (string, v1.Options) {
			id, decimals := rwaListedBoundEUTBL, map[string]uint32{rwaListedBoundEUTBL: 5}
			o.FiatPeggedClassics = map[string]canonical.Asset{id: eur}
			o.Sep1Cache, o.Directory = &stubSep1BoundReader{}, &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}}
			o.AssetsReader = &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: map[string][]timescale.AssetRow{}}
			o.RWAContracts = &stubRWAContractReader{accounts: 18000}
			o.RWAListings = &stubRWAListings{rows: []timescale.ListingEntry{listed(id, "eutbl", "eutbl", "1.22")}}
			o.ContractCatalogue = &stubContractCatalogue{rows: map[string]timescale.AssetRow{id: rwaContractRow(id, nil)}}
			o.TokenSymbol = &stubTokenSymbols{byID: map[string]string{}}
			o.TokenSupply = &stubTokenSupplies{byID: map[string]string{id: "28327867109034"}}
			o.TokenDecimals, o.NonstandardDecimals = &stubTokenDecimalsRdr{byID: decimals}, confirmedNonstandardDecimals(t, decimals)
			return "/v1/rwa/assets", o
		},
	}
}

// TestDeclaredPegFX_EveryRouteWithholdsAnExcludedRate: no route serves a declared-peg price built on an excluded or unjudgeable
// FX rate, and none presents the withheld body as a clean, cacheable answer.
func TestDeclaredPegFX_EveryRouteWithholdsAnExcludedRate(t *testing.T) {
	const rate = `"1.1734"`
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), InverseUSDText: "1.1734", Source: "ecb"},
	}}
	gates := map[string]v1.PriceExclusionGate{
		"ecb excluded": assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "ecb", Base: "fiat:EUR", Quote: "fiat:USD", From: time.Now().Add(-72 * time.Hour),
		}}}),
		"exclusions unread": assetExclGate(exclTable{err: errors.New("db down")}),
	}
	for name, route := range pegRoutes(t) {
		t.Run(name, func(t *testing.T) {
			path, o := route(v1.Options{FXHistory: fx})
			if _, plain := exclGet(t, v1.New(o), path); !strings.Contains(plain, rate) || !strings.Contains(plain, "declared_peg") {
				t.Fatalf("no gate: the peg fill did not price the member: %s", plain)
			}
			for gname, g := range gates {
				path, o := route(v1.Options{FXHistory: fx, Exclusions: g})
				resp := mustGet(t, startHTTPTest(t, v1.New(o).Handler()).URL+path)
				body, _ := readAll(resp)
				if cc := resp.Header.Get("Cache-Control"); strings.Contains(body, rate) || cc != "no-store" {
					t.Errorf("%s: Cache-Control %q, body %s; want the peg price withheld on a no-store body", gname, cc, body)
				}
			}
		})
	}
}
