package v1

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type creditFXStub struct{ points []FXQuotePoint }

func (s *creditFXStub) ListFXHistory(context.Context, string, time.Time, time.Time) ([]FXQuotePoint, error) {
	return s.points, nil
}

// TestCatalogueFiatRow_CarriesPriceProvenance: a fiat listing row's price_usd is one FX vendor's rate, so the row names
// its authority, source, time and vendor credit (E-7) instead of serving the value bare; the identity USD row carries no
// credit.
func TestCatalogueFiatRow_CarriesPriceProvenance(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	asOf := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	s := &Server{
		Options: Options{VerifiedCurrencies: cat, FXHistory: &creditFXStub{points: []FXQuotePoint{
			{Bucket: asOf, RateUSDText: "0.85", InverseUSDText: "1.17", Source: "massive"},
		}}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w := httptest.NewRecorder()
	s.handleExternalAssetList(w, httptest.NewRequest(http.MethodGet, "/v1/external/assets?asset_class=fiat", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var page struct {
		Data []AssetDetail `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rows := map[string]AssetDetail{}
	for _, row := range page.Data {
		rows[row.Slug] = row
	}
	eur, ok := rows["euro"]
	if !ok || eur.PriceUSD == nil || *eur.PriceUSD != "1.17" {
		t.Fatalf("euro row = %+v, want price_usd 1.17 from the FX stub", eur)
	}
	if eur.PriceAuthority != aggregate.AuthorityReferenceRate || !slices.Equal(eur.PriceSources, []string{"massive"}) ||
		eur.PriceAsOf == nil || !time.Time(*eur.PriceAsOf).Equal(asOf) || eur.PriceCredit != "Massive" {
		t.Errorf("euro provenance = authority %q sources %v as_of %v credit %q; want reference_rate/[massive]/%s/Massive",
			eur.PriceAuthority, eur.PriceSources, eur.PriceAsOf, eur.PriceCredit, asOf)
	}
	usd := rows["us-dollar"]
	if usd.PriceAuthority != aggregate.AuthorityIdentity || usd.PriceCredit != "" || usd.PriceAsOf != nil {
		t.Errorf("us-dollar provenance = authority %q credit %q as_of %v; want identity with no vendor credit and no clock stamp",
			usd.PriceAuthority, usd.PriceCredit, usd.PriceAsOf)
	}
}

// TestFXQuoteSourceLabel: a reader that carries no fx_quotes vendor label still names the table, never an empty source.
func TestFXQuoteSourceLabel(t *testing.T) {
	if got := fxQuoteSourceLabel(""); got != primaryFXSource {
		t.Errorf("empty source label = %q, want fx_quotes", got)
	}
	if got := fxQuoteSourceLabel("massive"); got != "massive" {
		t.Errorf("label = %q, want massive", got)
	}
}

// TestDivergence_CreditsLicensedReference: a reference row from a licensed aggregator carries `credit`; an on-chain
// oracle reference omits the key.
func TestDivergence_CreditsLicensedReference(t *testing.T) {
	at := time.Date(2026, 7, 3, 22, 37, 8, 0, time.UTC)
	s := withholdingServer()
	s.Divergences = &withholdingDivergenceReader{latest: []timescale.DivergenceRow{
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "redstone", ObservedAt: at, OurPrice: "100", RefPrice: "99", DeltaPct: "1", Status: "clear"},
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Reference: "coingecko", ObservedAt: at, OurPrice: "100", RefPrice: "100", DeltaPct: "0", Status: "clear"},
	}}
	rec := httptest.NewRecorder()
	s.handleDivergence(rec, httptest.NewRequest(http.MethodGet, "/v1/divergence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			Pairs []struct {
				References []map[string]json.RawMessage `json:"references"`
			} `json:"pairs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data.Pairs) != 1 || len(got.Data.Pairs[0].References) != 2 {
		t.Fatalf("want 1 pair with 2 references: %s", rec.Body.String())
	}
	for _, o := range got.Data.Pairs[0].References {
		var ref string
		_ = json.Unmarshal(o["reference"], &ref)
		raw, present := o["credit"]
		switch ref {
		case "coingecko":
			if string(raw) != `"CoinGecko"` {
				t.Errorf("coingecko credit = %s, want CoinGecko", raw)
			}
		case "redstone":
			if present {
				t.Errorf("redstone carries credit %s; an on-chain oracle has none", raw)
			}
		}
	}
}
