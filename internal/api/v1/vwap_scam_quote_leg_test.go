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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Regression suite for F002/F019/F032/T039: the scam-issuer gate was
// keyed on the BASE leg alone, so a flagged issuer's withheld price was
// republished — exactly, as its reciprocal — by moving the asset to the
// QUOTE side of the same request.
//
// /v1/vwap?base=native&quote=<FLAGGED> is unauthenticated, computes the
// number from the flagged issuer's own trades, and answered 200 while
// /v1/vwap?base=<FLAGGED>&quote=native answered 404 price-withheld. The
// two requests describe the SAME market; publishing one and withholding
// the other is not a gate.
//
// The gate is deliberately exercised through the REAL
// *pricingguard.ScamGate over a stub directory rather than through a
// hand-written fake with a `withheld map[string]bool`: a fake that
// answers only the base-only question would let this test pass without
// the pair-aware primitive, which is the vacuity this whole class came
// from.

// scamQuoteLegDirectory flags exactly the listed G-addresses and records
// every address it was asked about — what the directory is ASKED is the
// proof that the QUOTE leg reached the gate at all.
type scamQuoteLegDirectory struct {
	flagged map[string]bool
	asked   []string
}

func (d *scamQuoteLegDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	d.asked = append(d.asked, address)
	if d.flagged[address] {
		return timescale.DirectoryEntry{Address: address, Tags: []string{"unsafe"}}, true, nil
	}
	return timescale.DirectoryEntry{}, false, nil
}

const scamQuoteLegIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func TestVWAPWithholdsWhenTheQuoteLegIsScamFlagged(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	// Trades exist for the inverted orientation, so an ungated handler
	// serves a real 200 with a price — without them this would 404 for
	// lack of data and pass vacuously.
	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + flagged.String(): {scamTestTrade(t, native, flagged)},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote="+flagged.String())
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/vwap?base=native&quote=%s status = %d, want 404 — swapping the legs "+
			"republishes the withheld market's price as its exact reciprocal. Body: %s",
			flagged.String(), resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if len(dir.asked) == 0 {
		t.Fatal("the directory was never asked about any address — the QUOTE leg never " +
			"reached the gate, so a 404 here came from something other than the withholding decision")
	}
	if dir.asked[0] != scamQuoteLegIssuer {
		t.Errorf("directory asked about %v, want the QUOTE leg's issuer %q first",
			dir.asked, scamQuoteLegIssuer)
	}
}

// TestVWAPServesWhenNeitherLegIsFlagged is the blast-radius guard for the
// pair-aware gate: folding the quote leg in must not withhold ordinary
// markets.
func TestVWAPServesWhenNeitherLegIsFlagged(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {scamTestTrade(t, native, usdc)},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{}}
	srv := v1.New(v1.Options{History: reader, Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote="+usdc.String())
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/vwap for an unflagged pair returned %d, want 200 — the pair-aware "+
			"gate must withhold only flagged issuers. Body: %s", resp.StatusCode, body)
	}
}
