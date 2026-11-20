package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// requireStartsBeforeWindow asserts a timeframe=1h series against holedClock: one point at `first`, flagged as starting
// outside the window exactly when `want`.
func requireStartsBeforeWindow(t *testing.T, env chartEnvelope, first time.Time, want bool) {
	t.Helper()
	if len(env.Data.Points) != 1 || !env.Data.Points[0].T.Time().Equal(first) {
		t.Fatalf("points = %+v, want one at %s", env.Data.Points, first)
	}
	if env.Data.StartsBeforeWindow != want || env.Data.Truncated {
		t.Errorf("starts_before_window = %v (want %v), truncated = %v (want false)", env.Data.StartsBeforeWindow, want, env.Data.Truncated)
	}
	requested := holedClock().Add(-time.Hour)
	switch {
	case want && (env.Data.RequestedFrom == nil || !env.Data.RequestedFrom.Time().Equal(requested)):
		t.Errorf("requested_from = %v, want %s", env.Data.RequestedFrom, requested)
	case !want && env.Data.RequestedFrom != nil:
		t.Errorf("requested_from = %v on a series inside its window", env.Data.RequestedFrom)
	}
}

// A daily market-cap series asked for timeframe=1h serves yesterday's closed day, which ends before the window opens.
func TestChart_MarketCap1h_StartsBeforeWindow(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	sup := &stubSupplyLooker{}
	for n := 3; n >= 0; n-- {
		sup.daily = append(sup.daily, timescale.SupplyDayPoint{Bucket: holedDay(n), Circulating: big.NewInt(1_000_0000000)})
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		Clock:              holedClock,
		History:            newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": {holedDay(1): holedCEXPrice}}),
		Supply:             sup,
		USDPeggedClassics:  []canonical.Asset{usdc},
		VerifiedCurrencies: newTestCatalogue(t),
	}))
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1h&price_type=market_cap")
	requireStartsBeforeWindow(t, env, holedDay(1), true)
}

// A fiat series is daily, so timeframe=1h serves yesterday's fixing.
func TestChart_Fiat1h_StartsBeforeWindow(t *testing.T) {
	fx := &windowFXHistoryReader{points: []v1.FXQuotePoint{{Bucket: holedDay(1), RateUSDText: "0.92", InverseUSDText: "1.08"}}}
	ts := httpTestServer(t, v1.New(v1.Options{Clock: holedClock, History: &stubHistoryReader{}, FXHistory: fx}))
	resp := mustGet(t, ts.URL+"/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env chartEnvelope
	mustDecode(t, resp, &env)
	requireStartsBeforeWindow(t, env, holedDay(1), true)
}

// TWAP's last closed hour starts before a 1h window but overlaps it, so it is inside what was asked for.
func TestChart_TWAP1h_OverlappingBucketIsNotFlagged(t *testing.T) {
	bucket := holedClock().Truncate(time.Hour).Add(-time.Hour)
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": {bucket: holedCEXPrice}}))
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=1h&price_type=twap")
	requireStartsBeforeWindow(t, env, bucket, false)
}
