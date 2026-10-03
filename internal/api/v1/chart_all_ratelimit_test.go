package v1_test

import (
	"net/http"
	"testing"
)

// TestChartAll_ChargesByGranularity: timeframe=all has no window for
// chartFitGranularity to coarsen against, so the grain must select the
// price exactly as on /v1/history/since-inception; a bounded timeframe
// is already capped by coarsening and stays at the base token.
func TestChartAll_ChargesByGranularity(t *testing.T) {
	const pair = "/v1/chart?base=native&quote=fiat:USD"
	cases := []struct {
		name, query   string
		wantRemaining int
	}{
		{"all default grain", "&timeframe=all", 99},
		{"all 1d", "&timeframe=all&granularity=1d", 99},
		{"all 4h", "&timeframe=all&granularity=4h", 94},
		{"all 1m", "&timeframe=all&granularity=1m", 87},
		{"bounded 1m", "&timeframe=1h&granularity=1m", 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newSinceInceptionLimitedServer(t, 100)
			resp := mustGet(t, ts.URL+pair+tc.query)
			got := remainingBeforeProbe(t, resp, ts.URL+"/v1/chart")
			if got != tc.wantRemaining {
				t.Fatalf("remaining = %d, want %d (status %d)", got, tc.wantRemaining, resp.StatusCode)
			}
		})
	}
}

// TestChartAll_DeniedGrainDoesNoRead: the surcharge lands before the read.
func TestChartAll_DeniedGrainDoesNoRead(t *testing.T) {
	ts, reader := newSinceInceptionLimitedServer(t, 13)
	if resp := mustGet(t, ts.URL+"/v1/chart?base=native&quote=fiat:USD&timeframe=1h"); resp.StatusCode != http.StatusOK {
		t.Fatalf("bounded request status = %d, want 200", resp.StatusCode)
	}
	reader.oneMinuteReads.Store(0)
	resp := mustGet(t, ts.URL+"/v1/chart?base=native&quote=fiat:USD&timeframe=all&granularity=1m")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if n := reader.oneMinuteReads.Load(); n != 0 {
		t.Fatalf("store served %d 1m read(s) for a denied request", n)
	}
}
