package main

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// The stream's accepted windows must be exactly what is published: the aggregator's configured or default
// windows, plus the one-minute series only when its publisher runs.
func TestClosedStreamWindows(t *testing.T) {
	for name, tc := range map[string]struct {
		windows []string
		pairs   [][]string
		want    []int
	}{
		"defaults, no streaming pairs":  {want: []int{300, 3600, 86400}},
		"defaults with streaming pairs": {pairs: [][]string{{"native", "fiat:USD"}}, want: []int{60, 300, 3600, 86400}},
		"configured windows":            {windows: []string{"1h", "15m"}, want: []int{900, 3600}},
		"configured minute, with pairs": {windows: []string{"1m", "5m"}, pairs: [][]string{{"native", "fiat:USD"}}, want: []int{60, 300}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Aggregate.Windows = tc.windows
			cfg.API.Streaming.Pairs = tc.pairs
			got, err := closedStreamWindows(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("windows = %v, want %v", got, tc.want)
			}
		})
	}
}
