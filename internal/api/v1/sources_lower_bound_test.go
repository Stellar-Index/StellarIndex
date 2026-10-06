// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type lowerBoundSourcesStats struct {
	stats []timescale.SourceStats
	h24   []timescale.SourceVolumeBucket
}

func (s lowerBoundSourcesStats) GetSourceStats(context.Context) ([]timescale.SourceStats, error) {
	return s.stats, nil
}

func (s lowerBoundSourcesStats) GetSourceVolumeHistory24h(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return s.h24, nil
}

func (lowerBoundSourcesStats) GetSourceVolumeHistory7d(context.Context) ([]timescale.SourceVolumeBucket, error) {
	return nil, nil
}

// A source whose figure excludes unpriced XLM-leg trades serves its USD
// volume as a named lower bound; a fully priced source does not.
func TestSources_VolumeLowerBound(t *testing.T) {
	srv := v1.New(v1.Options{SourcesStats: lowerBoundSourcesStats{
		stats: []timescale.SourceStats{
			{Source: "soroswap", TradeCount24h: 10, UnpricedTrades24h: 3},
			{Source: "aquarius", TradeCount24h: 5},
		},
		h24: []timescale.SourceVolumeBucket{{Source: "phoenix", VolumeUSD: "1", XLMUnpriced: true}},
	}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/sources?include=stats,sparkline")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.Source `json:"data"`
	}
	mustDecode(t, resp, &body)
	got := map[string]bool{}
	for _, s := range body.Data {
		got[s.Name] = s.VolumeLowerBound
	}
	if !got["soroswap"] || !got["phoenix"] {
		t.Errorf("sources with excluded XLM legs must carry volume_lower_bound: %v", got)
	}
	if got["aquarius"] {
		t.Errorf("fully priced source must not be flagged: %v", got)
	}
}
