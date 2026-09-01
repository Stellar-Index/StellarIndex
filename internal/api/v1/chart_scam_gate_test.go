package v1_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// /v1/chart served a full price SERIES for a directory-scam-flagged
// issuer while /v1/price, /v1/price/tip, /v1/price/batch, /v1/vwap,
// /v1/twap, the SEP-40 oracle and the asset headline all withheld it
// (issue #366).
//
// This is the third time this exact class has appeared. MSP-02 found
// /v1/vwap and /v1/twap ungated after pricingguard/scam.go's own package
// doc claimed the gate sat "at the price-reader seam so every
// reader-backed surface is covered by ONE gate" — a claim that was never
// true, because those endpoints compute from raw trades via their own
// fetch and never touch the reader. /v1/chart is the same shape: it
// reads history directly.
//
// A series is arguably worse than a point. A single withheld price
// denies one number; an ungated chart hands over the whole trajectory,
// which is exactly what makes a manufactured market look legitimate.
//
// The gate is placed in handleChart BEFORE dispatchSpecialisedChart, so
// it covers the default path AND every specialised variant (market-cap,
// fiat-cross, TWAP) in one place. Gating each variant separately is how
// this class keeps recurring: a surface added later simply misses the
// check. These tests therefore exercise the variants explicitly — a
// gate that only covered the default path would pass a naive test and
// still leak three routes.

// chartScamGate withholds the listed bases and records the surface
// labels it was consulted with, so a test can assert the gate was
// actually asked — not merely that a 404 appeared for some other reason.
type chartScamGate struct {
	withheld map[string]bool
	surfaces []string
}

func (g *chartScamGate) Withheld(_ context.Context, base canonical.Asset, surface string) bool {
	g.surfaces = append(g.surfaces, surface)
	return g.withheld[base.String()]
}

const chartFlaggedIssuer = "RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func chartFlaggedBase(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(chartFlaggedIssuer)
	if err != nil {
		t.Fatalf("parse flagged base: %v", err)
	}
	return a
}

// TestChartWithholdsScamFlaggedIssuer is the headline case.
func TestChartWithholdsScamFlaggedIssuer(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: &pairAwareHistoryReader{}, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?base="+base.String()+"&quote=native&timeframe=24h")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/chart status = %d, want 404 — a scam-flagged issuer's price "+
			"SERIES must be withheld exactly as its point price is on /v1/price and "+
			"/v1/vwap (#366). Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "chart" {
		t.Errorf("gate surfaces = %v, want the first to be \"chart\" — the surface "+
			"label is how an operator attributes a withholding decision", gate.surfaces)
	}
}

// TestChartWithholdsAcrossEverySpecialisedVariant is the test that
// matters for the CLASS. handleChart dispatches to market-cap, fiat and
// TWAP handlers; a gate placed after that dispatch would leak all three
// while the headline test above still passed.
func TestChartWithholdsAcrossEverySpecialisedVariant(t *testing.T) {
	base := chartFlaggedBase(t)
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}

	for _, q := range []struct {
		name  string
		query string
	}{
		{"default vwap path", "base=" + base.String() + "&quote=native&timeframe=24h"},
		{"market_cap", "base=" + base.String() + "&quote=" + usd.String() + "&timeframe=24h&price_type=market_cap"},
		{"twap", "base=" + base.String() + "&quote=native&timeframe=24h&price_type=twap"},
		{"fiat cross", "base=" + base.String() + "&quote=" + usd.String() + "&timeframe=24h"},
	} {
		t.Run(q.name, func(t *testing.T) {
			gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
			srv := v1.New(v1.Options{History: &pairAwareHistoryReader{}, Scam: gate})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+"/v1/chart?"+q.query)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("variant %q returned %d, want 404 — the gate must sit BEFORE "+
					"the specialised dispatch so every chart shape is covered by one "+
					"check. Body: %s", q.name, resp.StatusCode, body)
			}
		})
	}
}

// TestChartGateKeysOnBaseNotQuote — the gate must key on the BASE alone
// so it survives the frontend's XLM triangulation. Keying on the pair
// would let a flagged asset through whenever it appeared against a
// different quote.
func TestChartGateKeysOnBaseNotQuote(t *testing.T) {
	base := chartFlaggedBase(t)
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: &pairAwareHistoryReader{}, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?base="+base.String()+"&quote="+usdc.String()+"&timeframe=24h")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a different quote returned %d, want 404 — the gate keys on the "+
			"BASE so it covers every quote", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestChartServesUnflaggedIssuer is the blast-radius guard. The gate
// must withhold ONLY flagged bases; if it 404s an ordinary pair it has
// broken the chart for everyone.
func TestChartServesUnflaggedIssuer(t *testing.T) {
	gate := &chartScamGate{withheld: map[string]bool{}} // wired, flags nothing
	srv := v1.New(v1.Options{History: &pairAwareHistoryReader{}, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?base=native&quote="+w2t2USDC+"&timeframe=24h")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "price-withheld") {
		t.Errorf("an UNFLAGGED pair was withheld — the gate must not blanket-close "+
			"the chart. Body: %s", body)
	}
}

// TestChartNilGateServes — a deployment with no gate wired must keep
// serving. Every other gate in this package is nil-safe and this call
// site must not be the exception.
func TestChartNilGateServes(t *testing.T) {
	srv := v1.New(v1.Options{History: &pairAwareHistoryReader{}}) // no Scam
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?base=native&quote="+w2t2USDC+"&timeframe=24h")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "price-withheld") {
		t.Errorf("a nil scam gate withheld the chart. Body: %s", body)
	}
}
