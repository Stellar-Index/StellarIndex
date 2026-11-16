package main

import (
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/orchestrator"
	"github.com/Stellar-Index/StellarIndex/internal/api/streampublish"
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// closedStreamWindows is every window_seconds a publisher feeds on /v1/price/stream: the aggregator's windows
// (the same `[aggregate].windows` it reads, built-in defaults when unset) and, when streaming pairs are
// configured, the API's own one-minute series.
func closedStreamWindows(cfg config.Config) ([]int, error) {
	windows, err := cfg.Aggregate.AggregatorWindows()
	if err != nil {
		return nil, err
	}
	if len(windows) == 0 {
		windows = orchestrator.DefaultWindows
	}
	var out []int
	for _, w := range windows {
		out = append(out, int(w/time.Second))
	}
	if len(cfg.API.Streaming.Pairs) > 0 {
		out = append(out, streampublish.WindowSeconds)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
