package v1_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// countingWithheldReader withholds every USD read with one gate verdict and counts the reads.
type countingWithheldReader struct {
	stubPriceReader
	verdict pricingguard.Withholding
	calls   atomic.Int64
}

func (r *countingWithheldReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.calls.Add(1)
	return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(r.verdict)
}

// TestAssetDetail_UnreadExclusionsNotCached: an unreadable exclusion table withholds price_usd for the request, but the body is a
// transient outage, so it is flagged stale and not replayed from the detail cache; an in-force exclusion is a lasting verdict
// and is cached as one.
func TestAssetDetail_UnreadExclusionsNotCached(t *testing.T) {
	for _, tc := range []struct {
		verdict   pricingguard.Withholding
		reason    string
		wantReads bool // a second request reads the price again
	}{
		{pricingguard.WithheldExclusionsUnread, "unattributed", true},
		{pricingguard.WithheldSourceExcluded, "source_excluded", false},
	} {
		reader := &countingWithheldReader{verdict: tc.verdict}
		usd, err := canonical.NewFiatAsset("USD")
		if err != nil {
			t.Fatal(err)
		}
		// A declared peg must not stand in for either verdict: both mean the price may rest on an excluded source.
		pegs := map[string]canonical.Asset{exclUSDC: usd}
		tsrv := startHTTPTest(t, v1.New(v1.Options{Prices: reader, FiatPeggedClassics: pegs}).Handler())
		var first int64
		for i := range 2 {
			resp := mustGet(t, tsrv.URL+"/v1/assets/"+exclUSDC)
			body, _ := readAll(resp)
			if strings.Contains(body, `"price_usd":"`) || !strings.Contains(body, `"price_withheld_reason":"`+tc.reason+`"`) {
				t.Fatalf("%s: %s, want price_usd withheld as %q", tc.verdict, body, tc.reason)
			}
			if got := strings.Contains(body, `"stale":true`); got != tc.wantReads {
				t.Fatalf("%s: stale=%v, want %v: %s", tc.verdict, got, tc.wantReads, body)
			}
			if i == 0 {
				first = reader.calls.Load()
			}
		}
		if first == 0 {
			t.Fatal(errors.New("the price reader was never consulted"))
		}
		if reread := reader.calls.Load() > first; reread != tc.wantReads {
			t.Errorf("%s: second request re-read the price = %v, want %v", tc.verdict, reread, tc.wantReads)
		}
	}
}
