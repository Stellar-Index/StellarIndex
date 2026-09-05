// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A bar's BaseVolume/QuoteVolume are sums of SMALLEST-UNIT amounts, and the
// smallest unit is a per-SOURCE scale (CS-040: on-chain DEX legs are
// 7-decimal stroops, CEX 8, the FX pollers 6). So a bar is a quantity in
// units only its contributing venues identify, and any caller that SUMS
// bars from different markets has to know those units or it adds
// incommensurable integers.
//
// internal/api/v1's fiat combine is that caller — on r1 today it merges
// native/<USDC classic> (sdex, 7dp) with crypto:XLM/crypto:USDT (binance,
// 8dp) into the same buckets. It could not correct for scale because the
// bar it received carried no attribution, even though the CAGG itself has
// stored `array_agg(DISTINCT source) AS sources` since migration 0002 and
// still does under 0147. These pin the column all the way out to OHLCBar.

func ohlcSourcesPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	quote, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("new pair: %v", err)
	}
	return p
}

var ohlcSourcesCols = []string{
	"bucket", "open", "high", "low", "close",
	"base_volume", "quote_volume", "trade_count", "sources",
}

// TestOHLCBarCarriesContributingSources is the behaviour half: a bucket the
// database attributes to two venues arrives with both, parsed.
func TestOHLCBarCarriesContributingSources(t *testing.T) {
	bucket := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)
	row := []driver.Value{
		bucket, "0.179", "0.1805", "0.1785", "0.1801",
		"181018642392870", "32465341612945", int64(2100),
		"{bitstamp,coinbase,kraken}",
	}

	for _, tc := range []struct {
		name string
		run  func(*Store) ([]OHLCBar, error)
	}{
		{
			name: "OHLCSeries",
			run: func(s *Store) ([]OHLCBar, error) {
				return s.OHLCSeries(context.Background(), ohlcSourcesPair(t), Granularity1h,
					bucket, bucket.Add(time.Hour), 10)
			},
		},
		{
			// The 5m/30m/2h/4h/12h/3d/2w intervals route here, and the
			// fiat combine reaches every one of them. A bar that arrives
			// unattributed from this reader would be silently exempt from
			// the scale lift, which is a half-fix.
			name: "OHLCSeriesReBucketed",
			run: func(s *Store) ([]OHLCBar, error) {
				return s.OHLCSeriesReBucketed(context.Background(), ohlcSourcesPair(t),
					Granularity1h, "4 hours", bucket, bucket.Add(4*time.Hour), 10)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newScriptedStore(t, scriptedResult{
				cols: ohlcSourcesCols,
				rows: [][]driver.Value{row},
			})
			bars, err := tc.run(store)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(bars) != 1 {
				t.Fatalf("%s returned %d bars, want 1", tc.name, len(bars))
			}
			got := bars[0].Sources
			want := []string{"bitstamp", "coinbase", "kraken"}
			if len(got) != len(want) {
				t.Fatalf("%s Sources = %v, want %v — a bar with no attribution "+
					"cannot be lifted to a common scale by a caller combining it "+
					"with another market", tc.name, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s Sources[%d] = %q, want %q", tc.name, i, got[i], want[i])
				}
			}
			// The numbers must be exactly what they always were: carrying
			// attribution is additive, never a change to a served value.
			if bars[0].BaseVolume != "181018642392870" || bars[0].TradeCount != 2100 {
				t.Errorf("%s base_volume/trade_count = %q/%d, want 181018642392870/2100",
					tc.name, bars[0].BaseVolume, bars[0].TradeCount)
			}
		})
	}
}

// TestOHLCBarSourcesSpanBothStoredDirections is the query-shape half. The
// scripted driver replays whatever the script says regardless of the SQL,
// so only reading the statement can prove the column was asked for AND that
// both stored directions of the market feed it. A read that unioned only
// the requested direction would resolve a market's scale from half its
// venues — and it is the SDEX half, the coarser one, that the decoder puts
// on whichever side it puts it.
func TestOHLCBarSourcesSpanBothStoredDirections(t *testing.T) {
	bucket := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		run  func(*Store) error
	}{
		{
			name: "OHLCSeries",
			run: func(s *Store) error {
				_, err := s.OHLCSeries(context.Background(), ohlcSourcesPair(t), Granularity1h,
					bucket, bucket.Add(time.Hour), 10)
				return err
			},
		},
		{
			name: "OHLCSeriesReBucketed",
			run: func(s *Store) error {
				_, err := s.OHLCSeriesReBucketed(context.Background(), ohlcSourcesPair(t),
					Granularity1h, "4 hours", bucket, bucket.Add(4*time.Hour), 10)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{cols: ohlcSourcesCols})
			if err := tc.run(store); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			stmts := conn.statements()
			if len(stmts) != 1 {
				t.Fatalf("%s issued %d statements, want 1", tc.name, len(stmts))
			}
			sql := stmts[0]
			if !strings.Contains(sql, "sources") {
				t.Fatalf("%s never selects the CAGG sources column:\n%s", tc.name, sql)
			}
			// Both stored directions must feed the union: the requested
			// orientation and the flipped one.
			for _, want := range []string{
				"FILTER (WHERE req)",
				"FILTER (WHERE NOT req)",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("%s folds sources from one direction only — missing %q:\n%s",
						tc.name, want, sql)
				}
			}
			// And the two arms are CONCATENATED, not picked between: a
			// market whose two spellings were traded by different venues
			// has to report both.
			if !strings.Contains(sql, "|| COALESCE(max(srcs) FILTER (WHERE NOT req)") {
				t.Errorf("%s does not concatenate the two directions' source arrays:\n%s",
					tc.name, sql)
			}
		})
	}
}
