// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// readCoverage returns the coverage published beside pair's VWAP, failing
// the test when the key is absent or unreadable.
func readCoverage(t *testing.T, mr *miniredis.Miniredis, pair canonical.Pair, window time.Duration) cachekeys.WindowCoverage {
	t.Helper()
	raw, err := mr.Get(cachekeys.VWAPCoverage(pair.Base, pair.Quote, window).String())
	if err != nil {
		t.Fatalf("coverage key: %v", err)
	}
	c, err := cachekeys.ParseVWAPCoverage(raw)
	if err != nil {
		t.Fatalf("coverage value %q: %v", raw, err)
	}
	return c
}

// TestDecideBucket_TruncatedWindowStatesCoveredSpan: a 24h window whose
// trade read hits the row cap inside its last 2h must publish, with the
// value, that it is truncated and the oldest trade the read reached.
// Before, the cap only counted and logged, and the value was published
// as if it covered the whole day.
func TestDecideBucket_TruncatedWindowStatesCoveredSpan(t *testing.T) {
	const window = 24 * time.Hour
	tickTime := time.Date(2026, 10, 6, 12, 0, 30, 0, time.UTC)
	bucketEnd := tickTime.Truncate(time.Minute)
	at := func(minAgo int) time.Time { return bucketEnd.Add(-time.Duration(minAgo) * time.Minute) }
	trade := func(ts time.Time) canonical.Trade {
		return buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), ts)
	}

	t.Run("direct pair over the cap", func(t *testing.T) {
		pair := xlmUsdtPair(t)
		// Six trades in the last 2h, cap 4: the read keeps the newest four,
		// so the oldest included is 80 minutes before the bucket end.
		store := &mockStore{trades: []canonical.Trade{
			trade(at(115)), trade(at(100)), trade(at(80)), trade(at(60)), trade(at(40)), trade(at(20)),
		}}
		rdb, mr := newTestRedis(t)
		stream := &recordingStreamPublisher{}
		o := New(store, rdb, Config{
			Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window},
			MaxTradesPerWindow: 4, StreamPublisher: stream,
		})
		o.clock = func() time.Time { return tickTime }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}

		want := cachekeys.WindowCoverage{Truncated: true, CoveredFrom: at(80)}
		if got := readCoverage(t, mr, pair, window); got != want {
			t.Errorf("coverage = %+v, want %+v", got, want)
		}
		if len(stream.calls) != 1 || stream.calls[0].coverage == nil || *stream.calls[0].coverage != want {
			t.Errorf("streamed coverage = %+v, want %+v on the one event", stream.calls, want)
		}

		// A replaying tick inside the same bucket republishes the decided
		// coverage with the value, not a default.
		mr.Del(cachekeys.VWAPCoverage(pair.Base, pair.Quote, window).String())
		o.clock = func() time.Time { return tickTime.Add(20 * time.Second) }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("replay Tick: %v", err)
		}
		if got := readCoverage(t, mr, pair, window); got != want {
			t.Errorf("replayed coverage = %+v, want %+v", got, want)
		}
	})

	t.Run("direct pair under the cap is complete", func(t *testing.T) {
		pair := xlmUsdtPair(t)
		store := &mockStore{trades: []canonical.Trade{trade(at(100)), trade(at(20))}}
		rdb, mr := newTestRedis(t)
		o := New(store, rdb, Config{
			Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window}, MaxTradesPerWindow: 4,
		})
		o.clock = func() time.Time { return tickTime }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if got := readCoverage(t, mr, pair, window); got != (cachekeys.WindowCoverage{}) {
			t.Errorf("coverage = %+v, want complete", got)
		}
	})

	t.Run("proxy pairs take the newest truncation point", func(t *testing.T) {
		xlm, _ := canonical.NewCryptoAsset("XLM")
		usd, _ := canonical.NewFiatAsset("USD")
		target, _ := canonical.NewPair(xlm, usd)
		usdt := func(ts time.Time) canonical.Trade {
			return backerTrade(t, "USDT", "binance", big.NewInt(100_000_000), big.NewInt(20_000_000), ts)
		}
		usdc := func(ts time.Time) canonical.Trade {
			return backerTrade(t, "USDC", "sdex", big.NewInt(10_000_000), big.NewInt(2_000_000), ts)
		}
		// Cap 2: USDT reaches back to 60m, USDC only to 30m, and the DAI
		// leg is under the cap. Every trade after 30m is in the value;
		// before it, USDC's are missing.
		store := &mockStore{perPair: map[string][]canonical.Trade{
			"crypto:XLM/crypto:USDT": {usdt(at(90)), usdt(at(60)), usdt(at(10))},
			"crypto:XLM/crypto:USDC": {usdc(at(50)), usdc(at(30)), usdc(at(5))},
			"crypto:XLM/crypto:DAI":  {backerTrade(t, "DAI", "sdex", big.NewInt(10_000_000), big.NewInt(2_000_000), at(200))},
		}}
		rdb, mr := newTestRedis(t)
		o := New(store, rdb, Config{
			Pairs: []canonical.Pair{target}, Windows: []time.Duration{window},
			MaxTradesPerWindow: 2, EnableStablecoinFiatProxy: true,
		})
		o.clock = func() time.Time { return tickTime }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		want := cachekeys.WindowCoverage{Truncated: true, CoveredFrom: at(30)}
		if got := readCoverage(t, mr, target, window); got != want {
			t.Errorf("coverage = %+v, want %+v", got, want)
		}
	})
}

// TestReseedFrozenVWAP_RestoresHeldCoverage: a reseeded held value
// carries the coverage it was published with, and a value whose coverage
// was never recorded gets none (unknown) rather than "complete".
func TestReseedFrozenVWAP_RestoresHeldCoverage(t *testing.T) {
	pair := xlmUsdtPair(t)
	const window = time.Hour
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	held := cachekeys.WindowCoverage{Truncated: true, CoveredFrom: end.Add(-20 * time.Minute)}
	for _, tc := range []struct {
		name  string
		known bool
	}{{"recorded", true}, {"never recorded", false}} {
		t.Run(tc.name, func(t *testing.T) {
			rdb, mr := newTestRedis(t)
			o := New(nil, rdb, Config{Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window}})
			stateKey := pair.String() + ":" + window.String()
			o.setComparator(stateKey, big.NewRat(1, 10), end, end)
			if tc.known {
				o.prevVWAPCoverage[stateKey] = held
			}
			o.reseedFrozenVWAP(context.Background(), pair, window, stateKey, 30*time.Minute)

			covKey := cachekeys.VWAPCoverage(pair.Base, pair.Quote, window).String()
			if !tc.known {
				if mr.Exists(covKey) {
					t.Error("coverage written for a held value whose coverage was never recorded")
				}
				return
			}
			if got := readCoverage(t, mr, pair, window); got != held {
				t.Errorf("reseeded coverage = %+v, want %+v", got, held)
			}
		})
	}
}
