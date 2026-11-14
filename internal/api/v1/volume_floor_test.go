package v1_test

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestVolumeFloors_AreFlagged: a USD volume that leaves unpriced trades out is served as a lower bound, and only then.
func TestVolumeFloors_AreFlagged(t *testing.T) {
	vol := "100"
	for _, floor := range []bool{true, false} {
		srv := v1.New(v1.Options{
			NetworkStats: &stubNetworkStatsReader{stats: timescale.NetworkStats{Volume24hUSD: &vol, VolumeLowerBound: floor}},
			Aggregators: &stubAggregatorsReader{rows: []timescale.AggregatorRollupRow{{
				Name: "soroswap-router", Kind: "router", RoutedTrades: 2, RoutedVolume: &vol, VolumeLowerBound: floor,
			}}},
		})
		ts := startHTTPTest(t, srv.Handler())
		for _, path := range []string{"/v1/network/stats", "/v1/aggregators"} {
			body, _ := readAll(mustGet(t, ts.URL+path))
			if got := strings.Contains(body, `"volume_lower_bound":true`); got != floor {
				t.Errorf("%s: volume_lower_bound present = %v, want %v (body=%s)", path, got, floor, body)
			}
			if !floor && strings.Contains(body, "volume_lower_bound") {
				t.Errorf("%s: the flag must be absent when nothing was excluded", path)
			}
			if !json.Valid([]byte(body)) {
				t.Errorf("%s: invalid JSON", path)
			}
		}
	}
}
