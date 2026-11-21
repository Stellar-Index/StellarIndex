package v1_test

import (
	"testing"
	"time"
)

// A VWAP chart whose granularity is as wide as its timeframe holds no closed bucket inside the window, so it must serve the last
// closed one rather than an empty series.
func TestChart_VWAPGrainAsWideAsWindowServesTheLastClosedBucket(t *testing.T) {
	day := holedClock().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	hour := holedClock().Truncate(time.Hour).Add(-time.Hour)
	ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": {day: holedCEXPrice, hour: holedCEXPrice}}))
	for _, tc := range []struct {
		query string
		want  time.Time
	}{
		{"timeframe=24h&granularity=1d", day},
		{"timeframe=1h&granularity=1h", hour},
	} {
		env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&"+tc.query)
		if len(env.Data.Points) == 0 || !env.Data.Points[0].T.Time().Equal(tc.want) {
			t.Errorf("%s: points = %+v, want the closed bucket at %s first", tc.query, env.Data.Points, tc.want)
		}
	}
}
