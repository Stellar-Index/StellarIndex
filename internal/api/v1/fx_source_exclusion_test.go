package v1_test

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fxExclClock strips the response clock and the held-value stub's lookup-time stamp; everything else must match.
var fxExclClock = regexp.MustCompile(`"(as_of|observed_at)":"[^"]*"`)

// TestFXSourceExclusionWithholdsTheFXLeg: every FX conversion (closed fixings, the tip's live snapshot, a frozen leg's held
// value converted at a fixing) is judged against the exclusions on the feed that published its rate.
func TestFXSourceExclusionWithholdsTheFXLeg(t *testing.T) {
	cases := []struct {
		path string
		opts v1.Options
	}{
		{path: "/v1/price?asset=fiat:EUR&quote=fiat:USD", opts: v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}}},
		{path: "/v1/price?asset=fiat:EUR&quote=fiat:NGN", opts: v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}}},
		{path: "/v1/price/tip?asset=fiat:EUR&quote=fiat:USD", opts: v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}}},
		{path: "/v1/price?asset=native&quote=fiat:BRL", opts: v1.Options{Prices: brlLegReader("binance", "sdex")}},
		{path: "/v1/price/tip?asset=native&quote=fiat:BRL", opts: v1.Options{Prices: brlLegReader("binance", "sdex")}},
		{path: "/v1/price?asset=native&quote=fiat:BRL", opts: v1.Options{
			Prices: brlLegReader("binance", "sdex"), Freeze: frozenPairs{xlmUSDKey: true}, Triangulated: lkgPairs{xlmUSDKey + "/300": legHeld},
		}},
	}
	// Before the fixings' bars: an exclusion that starts after a bar ended does not cover it.
	from := time.Now().Add(-48 * time.Hour)
	gate := func(e exclTable) v1.PriceExclusionGate {
		return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
	}
	// Pair-scoped to the feed's own readings, so only the FX check (not a cached-value rule on the USD leg) can fire.
	ecbExcluded := gate(exclTable{rows: []timescale.SourceExclusion{
		{ID: 1, Source: "ecb", Base: "fiat:EUR", Quote: "fiat:USD", From: from},
		{ID: 2, Source: "ecb", Base: "fiat:BRL", Quote: "fiat:USD", From: from},
	}})
	currencies, fixings := standbyCurrencies(), standbyFixings()
	for _, tc := range cases {
		newSrv := func(g v1.PriceExclusionGate) *v1.Server {
			opts := tc.opts
			opts.Currencies, opts.FXFixings, opts.Exclusions = currencies, fixings, g
			return v1.New(opts)
		}
		status, baseline := exclGet(t, newSrv(nil), tc.path)
		if status != http.StatusOK {
			t.Fatalf("%s: ungated %d %s, want 200", tc.path, status, baseline)
		}
		_, clean := exclGet(t, newSrv(gate(exclTable{})), tc.path)
		if fxExclClock.ReplaceAllString(clean, "") != fxExclClock.ReplaceAllString(baseline, "") {
			t.Fatalf("%s: no exclusions %s, want the ungated body %s", tc.path, clean, baseline)
		}
		status, body := exclGet(t, newSrv(ecbExcluded), tc.path)
		if status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) || strings.Contains(body, `"price":`) {
			t.Errorf("%s: ecb excluded %d %s, want 404 source_excluded with no price", tc.path, status, body)
		}
		status, body = exclGet(t, newSrv(gate(exclTable{err: errors.New("db down")})), tc.path)
		if status == http.StatusOK || strings.Contains(body, `"price":`) || strings.Contains(body, `"source_excluded"`) {
			t.Errorf("%s: exclusions unread %d %s, want a transient refusal, never a price or a clean verdict", tc.path, status, body)
		}
	}
}
