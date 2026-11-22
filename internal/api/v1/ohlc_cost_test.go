// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// readCountingHistory counts every per-book raw-trade and series read the handler issues.
type readCountingHistory struct {
	stubHistoryReader
	reads atomic.Int64
}

func (r *readCountingHistory) TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	r.reads.Add(1)
	return r.stubHistoryReader.TradesInRange(ctx, p, from, to, limit)
}

func (r *readCountingHistory) OHLCSeries(ctx context.Context, p canonical.Pair, interval string, from, to time.Time, limit int) ([]v1.OHLCSeriesBar, error) {
	r.reads.Add(1)
	return r.stubHistoryReader.OHLCSeries(ctx, p, interval, from, to, limit)
}

// TestFiatRawReads_ChargeEveryConstituentRead: a fiat-quoted raw-trade read (/v1/vwap, /v1/twap, point /v1/ohlc) scans
// trades once per fiat constituent, so it is charged at least one token per book read; and the charge lands before the
// read, so a request the remaining budget cannot cover reads nothing. A series read is charged for the rows it aggregates
// across every book instead (ohlc_series_cost_test.go).
func TestFiatRawReads_ChargeEveryConstituentRead(t *testing.T) {
	const budget = 30
	for _, path := range []string{
		"/v1/vwap?base=native&quote=fiat:USD",
		"/v1/twap?base=native&quote=fiat:USD",
		"/v1/ohlc?base=native&quote=fiat:USD",
	} {
		t.Run(path, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			reader := &readCountingHistory{}
			ts := startHTTPTest(t, v1.New(v1.Options{
				History: reader,
				RateLimit: middleware.RateLimitBySubject(
					ratelimit.New(rdb, budget, time.Minute, pinnedWindow), nil, middleware.SkipHealthAndMetrics, nil),
			}).Handler())

			if resp := mustGet(t, ts.URL+path); resp.StatusCode == http.StatusTooManyRequests {
				t.Fatal("first request into a fresh window was refused")
			}
			reads := reader.reads.Load()
			if reads < 2 {
				t.Fatalf("handler issued %d book reads; the fiat fan-out did not run", reads)
			}
			probe := mustGet(t, ts.URL+"/v1/ohlc")
			left, err := strconv.Atoi(probe.Header.Get("X-RateLimit-Remaining"))
			if err != nil {
				t.Fatalf("probe X-RateLimit-Remaining: %v", err)
			}
			if charged := int64(budget - (left + 1)); charged < reads {
				t.Fatalf("charged %d tokens for %d book reads", charged, reads)
			}

			// What is left cannot cover the same request again: it is refused before any read.
			if left+1 < int(reads) {
				if resp := mustGet(t, ts.URL+path); resp.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("unaffordable repeat: status %d, want 429", resp.StatusCode)
				}
				if again := reader.reads.Load(); again != reads {
					t.Errorf("refused request still issued %d book reads", again-reads)
				}
			}
		})
	}
}
