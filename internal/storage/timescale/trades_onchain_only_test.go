// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRawTradeReads_ExcludeOffChainSources pins the on-chain-only rule
// of /v1/history and /v1/observations at the query: the exclusion list
// is bound into the statement itself, so a keyset page is filled from
// on-chain rows rather than thinned after the fetch.
func TestRawTradeReads_ExcludeOffChainSources(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	now := time.Now()

	reads := []struct {
		name string
		argN int
		run  func(*Store) error
	}{
		{"LatestTradePerSource", 4, func(s *Store) error {
			_, err := s.LatestTradePerSource(context.Background(), pair, "")
			return err
		}},
		{"TradesInRangeAfter", 11, func(s *Store) error {
			_, err := s.TradesInRangeAfter(context.Background(), pair,
				now.Add(-time.Hour), now, time.Time{}, 0, "", "", 0, 10)
			return err
		}},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
			if err := r.run(store); err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			stmt := conn.only(t)
			if !strings.Contains(stmt.sql, "source <> ALL(string_to_array($") {
				t.Errorf("%s SQL has no off-chain source exclusion:\n%s", r.name, stmt.sql)
			}
			list, _ := stmt.arg(t, r.argN).(string)
			set := map[string]bool{}
			for _, n := range strings.Split(list, ",") {
				set[n] = true
			}
			for _, cex := range []string{"binance", "kraken", "bitstamp", "coinbase"} {
				if !set[cex] {
					t.Errorf("%s: exchange %q not excluded (arg $%d = %q)", r.name, cex, r.argN, list)
				}
			}
			for _, onchain := range []string{"sdex", "soroswap", "aquarius"} {
				if set[onchain] {
					t.Errorf("%s: on-chain source %q excluded", r.name, onchain)
				}
			}
		})
	}
}
