package v1

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// withheldTierReader refuses tier 1 with err while refusing is set, then serves a clean VWAP.
type withheldTierReader struct {
	err      error
	refusing atomic.Bool
}

func newWithheldTierReader(err error) *withheldTierReader {
	r := &withheldTierReader{err: err}
	r.refusing.Store(true)
	return r
}

func (r *withheldTierReader) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	if r.refusing.Load() {
		return "", time.Time{}, 0, nil, false, r.err
	}
	return "0.9998", time.Now().Add(-time.Minute), 100, []string{"kraken"}, true, nil
}

func (r *withheldTierReader) LatestAggregatorPrices(context.Context, canonical.Asset, canonical.Asset, []string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (r *withheldTierReader) LookupTriangulated(context.Context, canonical.Asset, canonical.Asset, time.Duration) (string, time.Time, bool, error) {
	return "", time.Time{}, false, nil
}

// TestGlobalAssetHeadline_WithheldTierBlocksOnChainFallback: a headline tier refused by a source exclusion is not "no data",
// so the on-chain listing price (raw pooled prices_1m, which no exclusion judges) does not fill it; an unreadable exclusion
// set marks the body stale and is not replayed, and a plain miss still takes the listing price.
func TestGlobalAssetHeadline_WithheldTierBlocksOnChainFallback(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	listing := "0.4321"
	get := func(t *testing.T, reader aggregate.GlobalPriceReader) string {
		t.Helper()
		srv := New(Options{
			VerifiedCurrencies: cat,
			GlobalPrice:        reader,
			AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		resp, err := http.Get(ts.URL + "/v1/assets/usdc") //nolint:noctx // test-local server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		return string(b)
	}

	if body := get(t, newWithheldTierReader(nil)); !strings.Contains(body, `"price_usd":"0.4321"`) ||
		strings.Contains(body, "price_withheld_reason") {
		t.Fatalf("plain miss: %s, want the on-chain listing price and no reason", body)
	}

	excluded := get(t, newWithheldTierReader(&aggregate.TierWithheldError{}))
	if strings.Contains(excluded, "price_usd") || !strings.Contains(excluded, `"price_withheld_reason":"source_excluded"`) ||
		strings.Contains(excluded, `"stale":true`) {
		t.Fatalf("excluded: %s, want no price, reason source_excluded, not stale", excluded)
	}

	unread := newWithheldTierReader(&aggregate.TierWithheldError{Unread: true})
	srv := New(Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        unread,
		AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	var bodies [2]string
	for i := range bodies {
		resp, err := http.Get(ts.URL + "/v1/assets/usdc") //nolint:noctx // test-local server
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodies[i] = string(b)
		unread.refusing.Store(false)
	}
	if strings.Contains(bodies[0], "price_usd") || !strings.Contains(bodies[0], `"price_withheld_reason":"unattributed"`) ||
		!strings.Contains(bodies[0], `"stale":true`) {
		t.Fatalf("unread: %s, want no price, reason unattributed, stale", bodies[0])
	}
	if !strings.Contains(bodies[1], `"price_usd":"0.9998"`) || strings.Contains(bodies[1], "price_withheld_reason") {
		t.Fatalf("after the read recovers: %s, want the fresh tier-1 price, not a replayed refusal", bodies[1])
	}
}
