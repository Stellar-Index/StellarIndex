// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

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
