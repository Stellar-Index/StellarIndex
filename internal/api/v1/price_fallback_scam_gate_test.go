package v1_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The reader gate only fires on LatestPrice arms with a value; every other exit is ErrPriceNotFound, which routes into
// priceFallback, whose layer 1 re-serves the aggregator's cached VWAP with no gate check. A scam-flagged issuer's triangulated
// pair would then answer 200; layers 2-4 propagate withheld, so the gate belongs at the chain's entry, not inside one layer.

// fallbackScamGate is a pair-aware stub (as production's *pricingguard.ScamGate, pinned by
// TestProductionScamGateIsPairAware) that records the surface labels it was asked with, so a test can assert it was
// consulted.
type fallbackScamGate struct {
	withheld map[string]bool
	surfaces []string
}

func (g *fallbackScamGate) Withheld(_ context.Context, base canonical.Asset, surface string) bool {
	g.surfaces = append(g.surfaces, surface)
	return g.withheld[base.String()]
}

func (g *fallbackScamGate) WithheldPair(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	return g.Withheld(ctx, base, surface) || g.Withheld(ctx, quote, surface)
}

// fallbackFlaggedAsset is the directory-flagged classic asset, shaped like chart_scam_gate_test.go's fixture: the
// canonical parser demands a real-looking issuer, and the asset exists only in this test's gate map.
const fallbackFlaggedAsset = "RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func fallbackFlaggedBase(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(fallbackFlaggedAsset)
	if err != nil {
		t.Fatalf("parse flagged asset: %v", err)
	}
	return a
}

// cachedVWAPLooker is the aggregator's Redis VWAP cache: it answers
// every pair with the same value, which is what makes it the side door
// — it knows nothing about directory flags.
type cachedVWAPLooker struct {
	value string
	calls int
}

func (l *cachedVWAPLooker) LookupTriangulatedVWAP(
	_ context.Context, _, _ canonical.Asset, _ time.Duration,
) (v1.CachedVWAP, bool, error) {
	l.calls++
	return v1.CachedVWAP{Value: l.value, Triangulated: true, ObservedAt: time.Now().UTC()}, true, nil
}

// TestPriceFallbackWithholdsScamFlaggedBase is the headline case: the reader misses (ErrPriceNotFound, synthetic-fiat
// fast path) and the Redis cache holds a triangulated value for the flagged issuer's pair.
func TestPriceFallbackWithholdsScamFlaggedBase(t *testing.T) {
	base := fallbackFlaggedBase(t)
	gate := &fallbackScamGate{withheld: map[string]bool{base.String(): true}}
	looker := &cachedVWAPLooker{value: "0.00723"}
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: looker,
		Scam:         gate,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+base.String()+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/price status = %d, want 404 — the Redis VWAP fallback re-served a "+
			"directory-flagged issuer's aggregated price that the reader gate withholds "+
			"the moment a prices_1m row exists (RLT-350). Body: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "0.00723") {
		t.Errorf("the withheld price value leaked into the response body: %s", body)
	}
	if !strings.Contains(string(body), "scam_issuer") {
		t.Errorf("body missing the scam_issuer reason: a withheld price must say why: %s", body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type — a withheld pair must not be "+
			"reported as price-not-found (MSP-06): %s", body)
	}
	if len(gate.surfaces) == 0 {
		t.Error("the scam gate was never consulted on the fallback chain")
	}
}

// TestPriceFallbackWithholdsScamFlaggedQuote is the other orientation: withholding is a property of the market, so XLM
// priced in a flagged asset is the flagged market inverted, and the chain must ask the pair question.
func TestPriceFallbackWithholdsScamFlaggedQuote(t *testing.T) {
	quote := fallbackFlaggedBase(t)
	gate := &fallbackScamGate{withheld: map[string]bool{quote.String(): true}}
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "138.4"},
		Scam:         gate,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote="+quote.String())
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/price status = %d, want 404 — `?asset=native&quote=<FLAGGED>` served the "+
			"reciprocal of the withheld number through the fallback chain. Body: %s",
			resp.StatusCode, body)
	}
	if strings.Contains(string(body), "138.4") {
		t.Errorf("the withheld price value leaked into the response body: %s", body)
	}
}

// TestPriceFallbackServesUnflaggedPair is the non-regression half: only the flagged market is withheld; a gate refusing
// every fallback would pass the other tests and 404 the whole triangulated long tail.
func TestPriceFallbackServesUnflaggedPair(t *testing.T) {
	flagged := fallbackFlaggedBase(t)
	gate := &fallbackScamGate{withheld: map[string]bool{flagged.String(): true}}
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "0.1242"},
		Scam:         gate,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unflagged pair must still serve the "+
			"aggregator's triangulated VWAP. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"price":"0.1242"`) {
		t.Errorf("body missing the served price: %s", body)
	}
}

// TestPriceBatchFallbackWithholdsScamFlaggedBase: /v1/price/batch runs the same chain, so the flagged row is omitted
// rather than served from cache; batch omits withheld rows and never 404s the request.
func TestPriceBatchFallbackWithholdsScamFlaggedBase(t *testing.T) {
	base := fallbackFlaggedBase(t)
	gate := &fallbackScamGate{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "0.00723"},
		Scam:         gate,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+base.String()+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (batch omits withheld rows). Body: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "0.00723") {
		t.Errorf("/v1/price/batch served the flagged issuer's cached VWAP: %s", body)
	}
	if !strings.Contains(string(body), `"data":[]`) {
		t.Errorf("withheld row must be omitted from the batch: %s", body)
	}
}

// TestCachedVWAPSurfacesWithholdScamFlaggedMarket: every route that can answer from the aggregator's VWAP cache must reach the
// same verdict on the same market: /v1/price (via priceFallback layer 1), /v1/price/tip (gated in computeTip), SEP-40
// passthroughs. /v1/price/batch is absent on purpose: it omits withheld rows rather than 404ing and has its own test.
func TestCachedVWAPSurfacesWithholdScamFlaggedMarket(t *testing.T) {
	base := fallbackFlaggedBase(t)
	const cached = "0.00723"

	for _, surface := range []struct {
		name string
		path string
	}{
		{"/v1/price", "/v1/price?asset=" + base.String() + "&quote=fiat:USD"},
		{"/v1/price/tip", "/v1/price/tip?asset=" + base.String() + "&quote=fiat:USD"},
		{"/v1/oracle/lastprice", "/v1/oracle/lastprice?asset=" + base.String()},
		{"/v1/oracle/x_last_price", "/v1/oracle/x_last_price?base=" + base.String() + "&quote=fiat:USD"},
	} {
		t.Run(surface.name, func(t *testing.T) {
			gate := &fallbackScamGate{withheld: map[string]bool{base.String(): true}}
			srv := v1.New(v1.Options{
				Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
				Triangulated: &cachedVWAPLooker{value: cached},
				Scam:         gate,
			})
			ts := startHTTPTest(t, srv.Handler())

			resp := mustGet(t, ts.URL+surface.path)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s status = %d, want 404 — this route served the aggregator's cached "+
					"VWAP for a directory-flagged market (RLT-350). Body: %s",
					surface.name, resp.StatusCode, body)
			}
			if strings.Contains(string(body), cached) {
				t.Errorf("%s leaked the withheld price value: %s", surface.name, body)
			}
			if !strings.Contains(string(body), "errors/price-withheld") {
				t.Errorf("%s must report the withheld verdict, not a bare not-found: %s",
					surface.name, body)
			}
			if len(gate.surfaces) == 0 {
				t.Errorf("%s never consulted the scam gate", surface.name)
			}
		})
	}
}
