package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// chartGranularityFitCase is one (timeframe, requested grain) request
// and the grain the response must both READ and REPORT.
type chartGranularityFitCase struct {
	timeframe string
	requested string
	served    string
	why       string
}

// A request whose grid outruns the response cap is served at the
// finest grain that fits, and the response says so — while a request
// that fits is untouched at every grain.
//
// Both halves are asserted at once because either alone is a
// half-check: an echo test that never exercises a fitting pair would
// pass on a handler that coarsened everything, and a fitting-pair test
// alone is the pre-fix behaviour.
//
// `granularity` on the wire is the signal, and it is asserted TOGETHER
// with the grain the reader was called at: a response that echoed
// `15m` while reading `1m` would be a new lie in the field this change
// exists to make true, and the reverse (reading 15m, echoing 1m) is
// the bug itself.
func TestChart_CoarsensAGranularityTheWindowCannotCarry(t *testing.T) {
	for _, tc := range []chartGranularityFitCase{
		{
			timeframe: "1y", requested: "1m", served: "15m",
			why: "365d of minutes is 525,600 grid points against a 50,000-point cap",
		},
		{
			timeframe: "1y", requested: "15m", served: "15m",
			why: "35,040 points — fits, so it is served as asked",
		},
		{
			timeframe: "1mo", requested: "1m", served: "1m",
			why: "30d of minutes is 43,200 points — under the cap, untouched",
		},
		{
			timeframe: "1w", requested: "1m", served: "1m",
			why: "10,080 points",
		},
		{
			timeframe: "24h", requested: "1m", served: "1m",
			why: "1,440 points",
		},
		{
			timeframe: "1h", requested: "1m", served: "1m",
			why: "60 points",
		},
		{
			timeframe: "all", requested: "1m", served: "1m",
			why: "no requested width — the point count is a property of the data, not the request",
		},
		{
			timeframe: "1y", requested: "1d", served: "1d",
			why: "365 points; coarsening must never touch a grain that fits",
		},
	} {
		t.Run(tc.timeframe+"_"+tc.requested, func(t *testing.T) {
			reader := &stubHistoryReader{points: []v1.HistoryPoint{
				{Bucket: time.Unix(1_770_000_000, 0).UTC(), VWAP: "0.4200"},
			}}
			srv := v1.New(v1.Options{History: reader})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+
				"/v1/chart?asset=native&quote=fiat:USD&timeframe="+tc.timeframe+
				"&granularity="+tc.requested)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d want 200", resp.StatusCode)
			}
			var env struct {
				Data v1.ChartSeries `json:"data"`
			}
			mustDecode(t, resp, &env)

			if env.Data.Granularity != tc.served {
				t.Errorf("granularity on the wire = %q, want %q (%s)",
					env.Data.Granularity, tc.served, tc.why)
			}
			if reader.lastCall.granularity != tc.served {
				t.Errorf("reader was called at %q, want %q (%s)",
					reader.lastCall.granularity, tc.served, tc.why)
			}
			// The requested timeframe is what the caller asked for and
			// is echoed unchanged — coarsening narrows the RESOLUTION,
			// never the window.
			if env.Data.Timeframe != tc.timeframe {
				t.Errorf("timeframe = %q, want %q", env.Data.Timeframe, tc.timeframe)
			}
		})
	}
}

// A bad `?granularity=` still 400s with the served-set enumeration:
// the fit rule must not swallow an unknown grain by coarsening it onto
// a real one.
func TestChart_UnknownGranularityStill400sAtEveryTimeframe(t *testing.T) {
	for _, tf := range []string{"1h", "24h", "1w", "1mo", "1y", "all"} {
		reader := &stubHistoryReader{pointsErr: v1.ErrUnknownGranularity}
		srv := v1.New(v1.Options{History: reader})
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe="+tf+"&granularity=2h")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("timeframe=%s granularity=2h → status=%d, want 400", tf, resp.StatusCode)
		}
	}
}
