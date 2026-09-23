package v1_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// The thin-market substance gate's wire contract (2026-08-04 valuation
// incident): a withheld pair 404s with the DISTINCT
// ".../errors/price-withheld" problem type — never the generic
// price-not-found, and never a fallback-served price.

func TestPrice_Withheld_Distinct404Type(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing price-withheld problem type: %s", body)
	}
	if strings.Contains(string(body), "errors/price-not-found") {
		t.Errorf("withheld must not be reported as not-found: %s", body)
	}
}

// TestPrice_Withheld_SkipsFallbackChain — the read-time stablecoin
// proxy (one arm of priceFallback) must NOT rescue a withheld pair:
// falling back would re-serve the same substanceless market through a
// side door. The stub returns withheld on the direct read; if the
// handler ran priceFallback, the configured USD peg's snapshot would
// serve a 200.
func TestPrice_Withheld_SkipsFallbackChain(t *testing.T) {
	peg, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	srv := v1.New(v1.Options{
		Prices:            &stubPriceReader{err: v1.ErrPriceWithheld},
		USDPeggedClassics: []canonical.Asset{peg},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (withheld must not fall back to the stablecoin proxy)", resp.StatusCode)
	}
}

// stubSubstanceGate implements v1.PriceSubstanceGate for the tip path.
type stubSubstanceGate struct {
	allow    bool
	surfaces []string
}

func (g *stubSubstanceGate) Allowed(_ context.Context, _, _ canonical.Asset, surface string) bool {
	g.surfaces = append(g.surfaces, surface)
	return g.allow
}

func (g *stubSubstanceGate) Verdict(ctx context.Context, base, quote canonical.Asset, surface string) (allowed, measured bool) {
	return g.Allowed(ctx, base, quote, surface), true
}

func TestPriceTip_Withheld_Distinct404Type(t *testing.T) {
	// The reader HAS a snapshot for the pair — proving the tip verdict
	// comes from the gate, not from data absence.
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.12", PriceType: "vwap"},
		},
	}
	gate := &stubSubstanceGate{allow: false}
	srv := v1.New(v1.Options{Prices: reader, Substance: gate})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "tip" {
		t.Errorf("gate consulted with surfaces %v, want [tip ...]", gate.surfaces)
	}
}

func TestPriceTip_GateAllows_Serves(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.12", PriceType: "vwap"},
		},
	}
	srv := v1.New(v1.Options{Prices: reader, Substance: &stubSubstanceGate{allow: true}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200: %s", resp.StatusCode, body)
	}
}

// TestPriceTip_Withheld_ScamIssuerReasonInDetail (T683) — a scam-issuer
// withhold and a substance-gate withhold both 404 with the same problem
// TYPE, but the DETAIL text must name the gate that actually fired: pre-fix
// writePriceWithheldProblem hard-coded the substance gate's wording for
// every withheld cause, so a directory-flagged issuer's response claimed
// "trailing market activity is below the serve floor" — a reason that
// never fired. Driven through the real *pricingguard.ScamGate, following
// the /v1/vwap and /v1/price/tip quote-leg suites, so the pair-aware
// primitive is what's proved, not a hand-written fake.
func TestPriceTip_Withheld_ScamIssuerReasonInDetail(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/" + flagged.String(): {Price: "0.12", PriceType: "vwap"},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{
		Prices: reader,
		Scam:   pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote="+flagged.String())
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "directory-flagged issuer") {
		t.Errorf("scam-gate withheld detail must name the flagged issuer as the cause: %s", body)
	}
	if strings.Contains(string(body), "trailing market activity is below the serve floor") {
		t.Errorf("scam-gate withheld body must not reuse the substance gate's wording — the reason must discriminate: %s", body)
	}
}

// TestPriceBatch_Withheld_RowOmitted — the batch wire contract omits a
// withheld row exactly like a miss (no 500, no fallback serve).
func TestPriceBatch_Withheld_RowOmitted(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (batch omits, never fails, on withheld rows)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"data":[]`) {
		t.Errorf("withheld row must be omitted from the batch: %s", body)
	}
}
