package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

type fxPointsReader struct {
	points []FXQuotePoint
	err    error
}

func (f fxPointsReader) ListFXHistory(context.Context, string, time.Time, time.Time) ([]FXQuotePoint, error) {
	return f.points, f.err
}

// ageSWREntries moves every single-row entry's observation time back by d.
func ageSWREntries(c *CachedAssetsReader, d time.Duration) {
	c.single.mu.Lock()
	defer c.single.mu.Unlock()
	for _, e := range c.single.entries {
		e.at = e.at.Add(-d)
	}
}

// TestSWR_OverdueEntryIsReported: plain expiry is routine; an entry its refresh has not replaced is not current.
func TestSWR_OverdueEntryIsReported(t *testing.T) {
	hold := newUpstreamHold(2) // the refresh stays parked, so the second read is the expired entry
	t.Cleanup(hold.release)
	c := NewCachedAssetsReader(&swrAssetByIDUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, hold: hold}, time.Minute)

	ctx, rep := withReadReport(context.Background())
	if _, err := c.GetAssetByAssetID(ctx, "X"); err != nil {
		t.Fatal(err)
	}
	if rep.stale.Load() {
		t.Fatal("cold fetch reported stale")
	}
	ageSWREntries(c, time.Minute+time.Second)
	if _, err := c.GetAssetByAssetID(ctx, "X"); err != nil {
		t.Fatal(err)
	}
	if rep.stale.Load() {
		t.Fatal("an entry one second past its ttl was reported stale")
	}
	ageSWREntries(c, assetsRefreshBudget)
	if _, err := c.GetAssetByAssetID(ctx, "X"); err != nil {
		t.Fatal(err)
	}
	if !rep.stale.Load() {
		t.Error("an entry past its ttl and a refresh budget was not reported stale")
	}
	if _, err := c.GetAssetByAssetID(context.Background(), "X"); err != nil {
		t.Errorf("read with no report installed: %v", err)
	}
}

// TestFiatUSDPrice_ReportsOldAndUnreadRates: an FX point past the weekend-and-holiday age is stale; a failed read is unread.
func TestFiatUSDPrice_ReportsOldAndUnreadRates(t *testing.T) {
	now := time.Now().UTC()
	point := func(age time.Duration) []FXQuotePoint {
		return []FXQuotePoint{{Bucket: now.Add(-age), InverseUSDText: "1.08", RateUSDText: "0.92"}}
	}
	cases := []struct {
		name          string
		fx            fxPointsReader
		stale, unread bool
		ok            bool
	}{
		{"yesterday", fxPointsReader{points: point(24 * time.Hour)}, false, false, true},
		{"after a long weekend", fxPointsReader{points: point(fiatRateStaleAfter - time.Hour)}, false, false, true},
		{"a week old", fxPointsReader{points: point(fiatRateStaleAfter + time.Hour)}, true, false, true},
		{"read failed", fxPointsReader{err: errors.New("pool timeout")}, false, true, false},
	}
	for _, c := range cases {
		s := unreadTestServer(Options{FXHistory: c.fx})
		ctx, rep := withReadReport(context.Background())
		_, _, _, ok := s.fiatUSDPriceFor(ctx, "EUR")
		if ok != c.ok || rep.stale.Load() != c.stale || rep.unread.Load() != c.unread {
			t.Errorf("%s: ok=%v stale=%v unread=%v, want %v %v %v",
				c.name, ok, rep.stale.Load(), rep.unread.Load(), c.ok, c.stale, c.unread)
		}
	}
}

// TestGlobalAsset_FlagsFollowTheReads pins the handler: an old rate is served stale and cacheable, a failed read no-store.
func TestGlobalAsset_FlagsFollowTheReads(t *testing.T) {
	vc := &currency.VerifiedCurrency{Ticker: "EUR", Slug: "euro", Name: "Euro", Class: currency.ClassFiat}
	rate := func(age time.Duration) []FXQuotePoint {
		return []FXQuotePoint{{Bucket: time.Now().UTC().Add(-age), InverseUSDText: "1.08", RateUSDText: "0.92"}}
	}
	cases := []struct {
		name    string
		fx      fxPointsReader
		stale   bool
		noStore bool
	}{
		{"fresh", fxPointsReader{points: rate(time.Hour)}, false, false},
		{"old rate", fxPointsReader{points: rate(fiatRateStaleAfter + time.Hour)}, true, false},
		{"read failed", fxPointsReader{err: errors.New("pool timeout")}, true, true},
	}
	for _, c := range cases {
		s := unreadTestServer(Options{FXHistory: c.fx})
		rec := httptest.NewRecorder()
		s.handleGlobalAsset(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/euro", nil), vc)
		var body struct {
			Flags struct {
				Stale bool `json:"stale"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		noStore := rec.Header().Get("Cache-Control") == "no-store"
		if body.Flags.Stale != c.stale || noStore != c.noStore {
			t.Errorf("%s: stale=%v no-store=%v, want %v %v", c.name, body.Flags.Stale, noStore, c.stale, c.noStore)
		}
	}
}

// TestAssetDetail_OverdueRowIsStaleButCached: a detail built on an overdue catalogue row is marked stale and still cached, so
// a refresh that keeps failing does not also switch the detail cache off.
func TestAssetDetail_OverdueRowIsStaleButCached(t *testing.T) {
	hold := newUpstreamHold(2)
	t.Cleanup(hold.release)
	c := NewCachedAssetsReader(&swrAssetByIDUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, hold: hold}, time.Minute)
	s := unreadTestServer(Options{AssetsReader: c})
	asset := mustAsset(t, withheldTestAsset)

	var first AssetDetail
	s.applyAssetExtensionFields(context.Background(), &first, asset)
	if first.extensionStale {
		t.Fatal("cold reads marked the detail stale")
	}
	ageSWREntries(c, time.Minute+assetsRefreshBudget+time.Second)
	var d AssetDetail
	s.applyAssetExtensionFields(context.Background(), &d, asset)
	if !d.extensionStale || !assetDetailStale(&d, false, false) {
		t.Errorf("overdue row: extensionStale=%v stale=%v, want both", d.extensionStale, assetDetailStale(&d, false, false))
	}
	if transientlyDegraded(&d, false) {
		t.Error("overdue row kept the detail out of its cache")
	}
}

// TestAssetDetail_UnreadPegRateIsNotCached: a declared-peg asset whose FX read failed has no price because nothing answered.
func TestAssetDetail_UnreadPegRateIsNotCached(t *testing.T) {
	asset := mustAsset(t, withheldTestAsset)
	s := unreadTestServer(Options{
		FXHistory:          fxPointsReader{err: errors.New("pool timeout")},
		FiatPeggedClassics: map[string]canonical.Asset{asset.String(): {Type: canonical.AssetFiat, Code: "EUR"}},
	})
	ctx, rep := withReadReport(context.Background())
	d := AssetDetail{AssetID: asset.String()}
	s.fillDeclaredPegPrice(ctx, &d, nil)
	if d.PriceUSD != nil || !rep.unread.Load() {
		t.Errorf("price=%v unread=%v, want no price and unread", d.PriceUSD, rep.unread.Load())
	}
}
