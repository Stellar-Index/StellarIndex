package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

type fxPointsReader struct {
	points []FXQuotePoint
	err    error
}

func (f fxPointsReader) ListFXHistory(context.Context, string, time.Time, time.Time) ([]FXQuotePoint, error) {
	return f.points, f.err
}

// TestSWR_ExpiredEntryIsReported: a row answered from an expired cache entry tells the response it is not current.
func TestSWR_ExpiredEntryIsReported(t *testing.T) {
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
	expireSWREntries(c)
	if _, err := c.GetAssetByAssetID(ctx, "X"); err != nil {
		t.Fatal(err)
	}
	if !rep.stale.Load() {
		t.Error("read of an expired entry was not reported stale")
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
		{"six days old", fxPointsReader{points: point(6 * 24 * time.Hour)}, true, false, true},
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
		{"old rate", fxPointsReader{points: rate(6 * 24 * time.Hour)}, true, false},
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

// TestAssetDetail_ExpiredRowIsStaleAndNotCached: a detail built on an expired catalogue row is marked and kept out of the cache.
func TestAssetDetail_ExpiredRowIsStaleAndNotCached(t *testing.T) {
	d := &AssetDetail{extensionStale: true}
	if !assetDetailStale(d, false, false) || !transientlyDegraded(d, false) {
		t.Errorf("stale=%v transient=%v, want both", assetDetailStale(d, false, false), transientlyDegraded(d, false))
	}
}
