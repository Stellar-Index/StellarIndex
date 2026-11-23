// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type sourceStatsResult struct {
	SourcesStatsReader
	rows []timescale.SourceStats
	err  error
}

func (r sourceStatsResult) GetSourceStats(context.Context) ([]timescale.SourceStats, error) {
	return r.rows, r.err
}

// A health row whose counters could not be read says so; a counter that was read and found nothing is a true 0 and carries
// no marker.
func TestBuildSourceHealthMarksUnreadCounters(t *testing.T) {
	readErr := errors.New("prometheus timed out")
	for _, tc := range []struct {
		name       string
		status     *fakeStatusBackend
		stats      SourcesStatsReader
		wantUnread bool
	}{
		{"reads failed", &fakeStatusBackend{entriesErr: readErr}, sourceStatsResult{err: readErr}, true},
		{"readers unwired", nil, nil, true},
		{"read, nothing found", &fakeStatusBackend{sourceEntries24h: map[string]int64{}}, sourceStatsResult{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{SourcesStats: tc.stats}}
			if tc.status != nil {
				s.StatusBackend = tc.status
			}
			s.logger = discardLogger()
			for _, row := range buildSourceHealth(context.Background(), s) {
				if row.Name != "binance" {
					continue
				}
				raw, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"entries_24h_unmeasured", "trade_stats_unmeasured"} {
					if got := m[key] == true; got != tc.wantUnread {
						t.Errorf("%s = %v, want %v (row %s)", key, m[key], tc.wantUnread, raw)
					}
				}
				return
			}
			t.Fatal("no binance row")
		})
	}
}
