package v1_test

import (
	"net/http"
	"strconv"
	"testing"
)

// TestRawTradeRoutes_ChargeByWindow: the window decides how many trades chunks the read probes, so it decides the price.
// Each case is one request into a fresh 100-token window; the probe (a 400 costing the base token) reads what is left.
func TestRawTradeRoutes_ChargeByWindow(t *testing.T) {
	const pair = "?base=native&quote=fiat:USD"
	cases := []struct {
		name, query   string
		wantRemaining int
	}{
		{"default 1h window", pair, 99},
		{"7d is one chunk", pair + "&window=7d", 99},
		{"30d", pair + "&window=30d", 95},
		{"1y", pair + "&window=365d", 47},
		{"all of history spends the minute", pair + "&from=2015-01-01T00:00:00Z", 0},
		{"rejected before any charge", pair + "&window=30d&from=2015-01-01T00:00:00Z", 99},
	}
	for _, route := range []string{"/v1/vwap", "/v1/twap", "/v1/ohlc"} {
		for _, tc := range cases {
			t.Run(route+" "+tc.name, func(t *testing.T) {
				ts, _ := newSinceInceptionLimitedServer(t, 100)
				first := mustGet(t, ts.URL+route+tc.query)
				if first.StatusCode == http.StatusTooManyRequests {
					t.Fatalf("first request into a fresh window was refused")
				}
				probe := mustGet(t, ts.URL+route)
				if tc.wantRemaining == 0 {
					if probe.StatusCode != http.StatusTooManyRequests {
						t.Fatalf("probe status = %d, want 429 after a whole-history read", probe.StatusCode)
					}
					return
				}
				if probe.StatusCode != http.StatusBadRequest {
					t.Fatalf("probe status = %d, want 400", probe.StatusCode)
				}
				n, err := strconv.Atoi(probe.Header.Get("X-RateLimit-Remaining"))
				if err != nil {
					t.Fatalf("probe X-RateLimit-Remaining: %v", err)
				}
				if got := n + 1; got != tc.wantRemaining {
					t.Fatalf("remaining after request = %d, want %d", got, tc.wantRemaining)
				}
			})
		}
	}
}
