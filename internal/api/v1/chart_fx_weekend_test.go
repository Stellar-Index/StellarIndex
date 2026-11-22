// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestChart_Fiat_DeadFeedWindowStartIsStale: the session in force at `from` is served even when the feed stopped, but one that
// ended longer before the window than any no-session run is a dead feed's last rate, so it goes out stale and no-store; a
// weekend or the Easter run is not stale.
func TestChart_Fiat_DeadFeedWindowStartIsStale(t *testing.T) {
	wed := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	thu, fri := wed.AddDate(0, 0, 1), wed.AddDate(0, 0, 2)
	maundyThu := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC) // Good Friday 2026 is 3 April
	for _, tc := range []struct {
		name   string
		now    time.Time
		points []time.Time
		kept   time.Time
		stale  bool
	}{
		{"dead since Wednesday, read the following Friday", time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC), []time.Time{wed}, wed, true},
		{"Monday after a weekend", time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC), []time.Time{thu, fri}, fri, false},
		{"Tuesday after Easter", time.Date(2026, 4, 7, 10, 0, 0, 0, time.UTC), []time.Time{maundyThu}, maundyThu, false},
	} {
		pts := make([]v1.FXQuotePoint, len(tc.points))
		for i, b := range tc.points {
			pts[i] = v1.FXQuotePoint{Bucket: b, RateUSDText: "0.91", InverseUSDText: "1.09"}
		}
		now := tc.now
		ts := httpTestServer(t, v1.New(v1.Options{
			History: &stubHistoryReader{}, FXHistory: &windowFXHistoryReader{points: pts}, Clock: func() time.Time { return now },
		}))
		for _, q := range []string{
			"asset=fiat:EUR&quote=fiat:USD&timeframe=24h",
			"asset=fiat:EUR&quote=fiat:USD&timeframe=1h",
			"asset=fiat:EUR&quote=fiat:GBP&timeframe=24h",
		} {
			resp := mustGet(t, ts.URL+"/v1/chart?"+q)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status=%d", tc.name, q, resp.StatusCode)
			}
			var body struct {
				Data  v1.ChartSeries `json:"data"`
				Flags v1.Flags       `json:"flags"`
			}
			mustDecode(t, resp, &body)
			if len(body.Data.Points) != 1 || !time.Time(body.Data.Points[0].T).Equal(tc.kept) || !body.Data.StartsBeforeWindow {
				t.Errorf("%s %s: points=%+v starts_before_window=%v, want the %s session flagged", tc.name, q,
					body.Data.Points, body.Data.StartsBeforeWindow, tc.kept.Weekday())
				continue
			}
			if body.Flags.Stale != tc.stale {
				t.Errorf("%s %s: flags.stale = %v, want %v", tc.name, q, body.Flags.Stale, tc.stale)
			}
			if noStore := resp.Header.Get("Cache-Control") == "no-store"; noStore != tc.stale {
				t.Errorf("%s %s: Cache-Control = %q, want no-store=%v", tc.name, q, resp.Header.Get("Cache-Control"), tc.stale)
			}
		}
	}
}

// TestChart_Fiat_WeekendServesTheLastSession: fx_quotes holds no weekend rows, so on a Sunday or Monday a short fiat window
// must serve Friday's session, flagged as starting before the window, rather than an empty unflagged 200.
func TestChart_Fiat_WeekendServesTheLastSession(t *testing.T) {
	thu := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	fri := thu.AddDate(0, 0, 1)
	fx := &windowFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: thu, RateUSDText: "0.91", InverseUSDText: "1.09"},
		{Bucket: fri, RateUSDText: "0.92", InverseUSDText: "1.08"},
	}}
	for _, now := range []time.Time{
		time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC), // Sunday
		time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC), // Monday, before its row is closed
	} {
		ts := httpTestServer(t, v1.New(v1.Options{
			History: &stubHistoryReader{}, FXHistory: fx, Clock: func() time.Time { return now },
		}))
		for _, q := range []string{
			"asset=fiat:EUR&quote=fiat:USD&timeframe=24h",
			"asset=fiat:EUR&quote=fiat:USD&timeframe=1h",
			"asset=fiat:EUR&quote=fiat:GBP&timeframe=24h",
		} {
			resp := mustGet(t, ts.URL+"/v1/chart?"+q)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status=%d", now.Weekday(), q, resp.StatusCode)
			}
			var body struct {
				Data v1.ChartSeries `json:"data"`
			}
			mustDecode(t, resp, &body)
			if len(body.Data.Points) != 1 || !time.Time(body.Data.Points[0].T).Equal(fri) {
				t.Errorf("%s %s: points = %+v, want Friday's session only", now.Weekday(), q, body.Data.Points)
				continue
			}
			if !body.Data.StartsBeforeWindow {
				t.Errorf("%s %s: starts_before_window = false for a session that closed before the window", now.Weekday(), q)
			}
		}
	}
}
