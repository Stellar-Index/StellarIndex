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

// The side door into the withholding decision (RLT-350).
//
// /v1/price's reader gate (cmd/stellarindex-api's priceWithheld
// chokepoint) only fires on the arms of LatestPrice that HAVE a value:
// the closed-1m-bucket arm and the last-trade arm. Every other exit is
// ErrPriceNotFound — the synthetic-fiat fast path (a fiat:/crypto:
// quote never has a literal prices_1m row, so an asset whose only
// market is against a stablecoin misses there by construction), and the
// zero-trades exit. That 404 is exactly what routes the handler into
// priceFallback, whose FIRST layer re-serves the aggregator's cached
// VWAP for the same pair with no gate consultation at all.
//
// So for a directory-scam-flagged issuer's triangulated pair, /v1/price
// answered 200 with the flagged market's own price — the number the
// same endpoint withholds the moment a prices_1m row exists for it. The
// aggregator that writes that Redis value gates nothing on the write
// path either (its ScamGate is built for the price-alert evaluator
// alone), so the cache is not a laundered-clean source.
//
// Layers 2-4 of the chain already propagate a withheld verdict
// (tryStablecoinFiatProxy / tryUSDAnchoredFiatCross read through the
// GATED LatestPrice on the proxy pair) — layer 1 is the one that does
// not, which is why the gate belongs at the chain's entry rather than
// inside one layer.

// fallbackScamGate is a PAIR-AWARE stub (the production
// *pricingguard.ScamGate is pair-aware — pinned by
// TestProductionScamGateIsPairAware), so these tests exercise the same
// fold production takes rather than the base-only compatibility arm.
// It records the surface labels it was asked with, so a test can assert
// the gate was actually consulted and not that a 404 arrived for some
// unrelated reason.
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

// fallbackFlaggedAsset is the directory-flagged classic asset. Same
// shape as the fixture chart_scam_gate_test.go uses: a real-looking
// issuer address is what the canonical parser demands, and the asset
// only ever exists inside this test's gate map.
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

// TestPriceFallbackWithholdsScamFlaggedBase is the headline case: the
// reader misses (ErrPriceNotFound, the synthetic-fiat fast path) and
// the Redis cache holds a triangulated value for the flagged issuer's
// pair.
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
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type — a withheld pair must not be "+
			"reported as price-not-found (MSP-06): %s", body)
	}
	if len(gate.surfaces) == 0 {
		t.Error("the scam gate was never consulted on the fallback chain")
	}
}

// TestPriceFallbackWithholdsScamFlaggedQuote is the other orientation.
// The withholding decision is a property of the MARKET, so the price of
// XLM IN a flagged issuer's asset is the flagged market's price
// inverted (F002/F019). The chain must ask the PAIR question.
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

// TestPriceFallbackServesUnflaggedPair is the non-regression half: the
// fix must withhold the flagged market and NOTHING else. A gate that
// refused every fallback would pass the two tests above and 404 the
// entire triangulated long tail.
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

// TestPriceBatchFallbackWithholdsScamFlaggedBase — /v1/price/batch runs
// the same chain (resolveBatchRow → priceFallback), so the flagged row
// must be OMITTED rather than served from cache. Batch omits withheld
// rows; it never 404s the whole request.
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

// TestCachedVWAPSurfacesWithholdScamFlaggedMarket is the class guard:
// EVERY route that can answer out of the aggregator's VWAP cache must
// reach the same verdict about the same market. The leak was never
// about one handler — the cache is a shared primitive, and each route
// reaches it by its own path:
//
//   - /v1/price — the reader misses (ErrPriceNotFound) and the handler
//     runs priceFallback, whose layer 1 is the cache;
//   - /v1/price?window=N — dispatched from handlePrice BEFORE the
//     reader is consulted, straight to the per-window cache keys, so
//     neither the reader chokepoint nor the fallback gate can see it;
//   - /v1/price/tip — its own cache branch, gated at the top of
//     computeTip (already correct; here so a regression there fails
//     alongside its siblings rather than silently);
//   - the two SEP-40 passthroughs — the same priceFallback chain, and
//     the surface whose consumers are oracle integrators.
//
// /v1/price/batch is deliberately absent: its wire contract OMITS a
// withheld row rather than 404ing the request, and it has its own test.
func TestCachedVWAPSurfacesWithholdScamFlaggedMarket(t *testing.T) {
	base := fallbackFlaggedBase(t)
	const cached = "0.00723"

	for _, surface := range []struct {
		name string
		path string
	}{
		{"/v1/price", "/v1/price?asset=" + base.String() + "&quote=fiat:USD"},
		{"/v1/price?window=300", "/v1/price?window=300&asset=" + base.String() + "&quote=fiat:USD"},
		{"/v1/price?window=3600", "/v1/price?window=3600&asset=" + base.String() + "&quote=fiat:USD"},
		{"/v1/price?window=86400", "/v1/price?window=86400&asset=" + base.String() + "&quote=fiat:USD"},
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

// TestPriceWindowedWithholdsScamFlaggedQuote — both legs here too: the
// windowed price of XLM IN a flagged issuer's asset is the flagged
// market's own windowed price, inverted.
func TestPriceWindowedWithholdsScamFlaggedQuote(t *testing.T) {
	quote := fallbackFlaggedBase(t)
	gate := &fallbackScamGate{withheld: map[string]bool{quote.String(): true}}
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "138.4"},
		Scam:         gate,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?window=3600&asset=native&quote="+quote.String())
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/price?window=3600 status = %d, want 404 for a flagged QUOTE. Body: %s",
			resp.StatusCode, body)
	}
	if strings.Contains(string(body), "138.4") {
		t.Errorf("the withheld price value leaked through the windowed route: %s", body)
	}
}

// TestPriceWindowedServesUnflaggedPair is the non-regression half for
// the windowed route.
func TestPriceWindowedServesUnflaggedPair(t *testing.T) {
	flagged := fallbackFlaggedBase(t)
	gate := &fallbackScamGate{withheld: map[string]bool{flagged.String(): true}}
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "0.1242"},
		Scam:         gate,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?window=300&asset=native&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unflagged pair must still serve its windowed "+
			"VWAP. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"window_seconds":300`) {
		t.Errorf("body missing the windowed snapshot: %s", body)
	}
}
