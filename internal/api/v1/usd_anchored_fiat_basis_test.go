package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ADR-0053: a single-venue fiat book is one exchange's view; XLM/USD is
// aggregated across many. Where both exist, the USD-anchored derivation
// is the better-substantiated number and is served instead — flagged
// triangulated, with both legs on the wire.

type basisPair struct {
	price   string
	sources []string
}

// basisReader serves exactly the pairs it is given, each with its own
// venue list, so a test controls how deep each book is.
type basisReader struct{ pairs map[string]basisPair }

func (r *basisReader) LatestPrice(
	_ context.Context, a, q canonical.Asset,
) (v1.PriceSnapshot, []string, bool, error) {
	p, ok := r.pairs[a.String()+"/"+q.String()]
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return v1.PriceSnapshot{
		AssetID: a.String(), Quote: q.String(), Price: p.price, PriceType: "vwap",
	}, p.sources, false, nil
}

func (r *basisReader) RecentClosedSnapshots(
	_ context.Context, _, _ canonical.Asset, _ int,
) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

const thinCHFBook = "0.2"

// basisFixture: XLM/CHF from one venue at 0.2, XLM/USD from usdVenues,
// and a CHF fixing of 0.8 (so the derivation is 0.17794… × 0.8 = 0.14235…).
func basisFixture(usdVenues []string, opts v1.Options) *v1.Server {
	opts.Prices = &basisReader{pairs: map[string]basisPair{
		"native/fiat:CHF": {price: thinCHFBook, sources: []string{"kraken"}},
		"native/fiat:EUR": {price: "0.15312172801139768016", sources: []string{"binance", "kraken"}},
		"native/fiat:USD": {price: xlmUSDPrice, sources: usdVenues},
	}}
	if opts.FXFixings == nil {
		barEnd := time.Now().UTC().Add(-timescale.FXFixingLag).Truncate(time.Hour)
		opts.FXFixings = fixingsOf(hourlyFixing("CHF", "0.8", barEnd), hourlyFixing("EUR", "0.86", barEnd))
	}
	return v1.New(opts)
}

var deepUSDVenues = []string{"binance", "coinbase", "kraken"}

func TestPriceThinFiatBookYieldsToUSDAnchor(t *testing.T) {
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{}).Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:CHF")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	// 0.17794602537043946501 × 0.8 = 0.142356820296351572008
	if !strings.Contains(body, `"price":"0.14235682`) {
		t.Errorf("a single-venue CHF book must yield to XLM/USD × USD/CHF (~0.14236): %s", body)
	}
	for _, want := range []string{`"triangulated":true`, `"fx_as_of":`, `"fx_rate":"0.8"`, `"usd_leg":`, "coinbase", "massive"} {
		if !strings.Contains(body, want) {
			t.Errorf("derived basis response missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"single_source":true`) {
		t.Errorf("the served value rests on three USD venues, not one: %s", body)
	}
}

// A multi-venue fiat book is a real market at least as deep as one USD
// venue; deriving over it would replace measured data with an estimate.
func TestPriceMultiVenueFiatBookIsServedAsObserved(t *testing.T) {
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{}).Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:EUR")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "0.15312172801139768016") || strings.Contains(body, `"triangulated":true`) {
		t.Errorf("a multi-venue EUR book must be served as observed, not as the "+
			"derived cross (0.17794… × 0.86 = 0.1530…): %s", body)
	}
}

func TestPriceThinUSDLegDoesNotDisplaceThinBook(t *testing.T) {
	ts := startHTTPTest(t, basisFixture([]string{"kraken"}, v1.Options{}).Handler())

	assertServesThinCHFBook(t, ts.URL+"/v1/price?asset=native&quote=fiat:CHF",
		"a one-venue USD leg is no better substantiated than the one-venue book")
}

func TestPriceFiatBasisKillSwitchServesThinBook(t *testing.T) {
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{DisableFiatBasis: true}).Handler())

	assertServesThinCHFBook(t, ts.URL+"/v1/price?asset=native&quote=fiat:CHF",
		"pricing_guard.disable_fiat_basis must restore the direct book")
}

// A derivation that cannot bind its FX leg must leave the observed book
// in place, not turn a served price into a withheld one.
func TestPriceFiatBasisMissKeepsDirectBook(t *testing.T) {
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{FXFixings: fixingsOf()}).Handler())

	assertServesThinCHFBook(t, ts.URL+"/v1/price?asset=native&quote=fiat:CHF",
		"an unbindable FX leg must fall back to the direct book")
}

func TestPriceBatchThinFiatBookYieldsToUSDAnchor(t *testing.T) {
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{}).Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:CHF")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"price":"0.14235682`) || !strings.Contains(body, `"triangulated":true`) {
		t.Errorf("/v1/price/batch must apply the same basis as /v1/price: %s", body)
	}
}

// pairConfidence scores each pair on its own book, as the aggregator does.
type pairConfidence map[string]v1.PriceSnapshotConfidence

func (p pairConfidence) LookupConfidence(_ context.Context, a, q canonical.Asset, _ time.Duration) (v1.PriceSnapshotConfidence, bool, error) {
	c, ok := p[a.String()+"/"+q.String()]
	return c, ok, nil
}

// The thin book's confidence, composite and divergence verdicts describe
// the book that was replaced, not the derived value on the wire.
func TestPriceBasisRowCarriesNoThinBookEnrichment(t *testing.T) {
	conf := pairConfidence{
		"native/fiat:CHF": {Confidence: 0.31, Factors: v1.ConfidenceFactors{SourceCount: 0.1}},
		"native/fiat:EUR": {Confidence: 0.77, Factors: v1.ConfidenceFactors{SourceCount: 0.6}},
	}
	div := &stubDivergenceLooker{firing: true, checked: true}
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{Confidence: conf, Divergence: div}).Handler())

	// The looker is live: an observed book keeps its own enrichment.
	eur, _ := readAll(mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:EUR"))
	if !strings.Contains(eur, `"confidence":0.77`) || !strings.Contains(eur, `"divergence_warning":true`) {
		t.Fatalf("instrument check: the observed EUR book must carry its pair enrichment: %s", eur)
	}

	chf, _ := readAll(mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:CHF"))
	if !strings.Contains(chf, `"price":"0.14235682`) {
		t.Fatalf("precondition: CHF must be served from the USD anchor: %s", chf)
	}
	for _, banned := range []string{`"confidence":0.31`, `"source_count":0.1`, `"divergence_warning":true`, `"divergence_checked":true`} {
		if strings.Contains(chf, banned) {
			t.Errorf("triangulated row carries the displaced book's %s: %s", banned, chf)
		}
	}
}

// x_last_price is a closed-bucket surface too; it must serve the same
// number /v1/price does for the pair.
func TestOracleXLastPriceAppliesFiatBasis(t *testing.T) {
	ts := startHTTPTest(t, basisFixture(deepUSDVenues, v1.Options{}).Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/x_last_price?base=native&quote=fiat:CHF")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"price":"0.14235682`) || !strings.Contains(body, `"triangulated":true`) {
		t.Errorf("x_last_price must apply the same basis as /v1/price: %s", body)
	}
}

func assertServesThinCHFBook(t *testing.T, url, why string) {
	t.Helper()
	resp := mustGet(t, url)
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"price":"`+thinCHFBook+`"`) || strings.Contains(body, `"triangulated":true`) {
		t.Errorf("%s: %s", why, body)
	}
}
