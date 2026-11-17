// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// agedAnchorStore serves, per quote, one candidate bucket `age` before the requested instant, and a trailing
// baseline of twelve 1.0 buckets starting `trailingAge` before it (nil trailing = fetch fails, guard passes).
type agedAnchorStore struct {
	byQuote map[string]agedAnchor
}

type agedAnchor struct {
	age         time.Duration
	vwap        string
	trailingAge time.Duration
	trailing    bool
}

func (s *agedAnchorStore) ClosedVWAP1mAtOrBefore(_ context.Context, p canonical.Pair, at time.Time) (timescale.Vwap1mRow, error) {
	a, ok := s.byQuote[p.Quote.String()]
	if !ok {
		return timescale.Vwap1mRow{}, sql.ErrNoRows
	}
	return timescale.Vwap1mRow{Bucket: at.Add(-a.age).Truncate(time.Minute), VWAP: a.vwap, TradeCount: 5}, nil
}

func (s *agedAnchorStore) ClosedVWAP1mCombinedBefore(_ context.Context, p canonical.Pair, before time.Time, limit int) ([]timescale.Vwap1mRow, error) {
	a := s.byQuote[p.Quote.String()]
	if !a.trailing {
		return nil, errors.New("no trailing rows in this test")
	}
	start := before.Add(-a.trailingAge)
	rows := make([]timescale.Vwap1mRow, 0, limit)
	for i := 0; i < 12 && i < limit; i++ {
		rows = append(rows, timescale.Vwap1mRow{Bucket: start.Add(-time.Duration(i) * time.Minute), VWAP: "1.0", TradeCount: 5})
	}
	return rows, nil
}

// The 24h-ago anchor is the divisor of change_24h_pct, so a bucket older than the listing's 24h-window tolerance
// is no anchor on any arm: the literal USD pair, the peg walk, or the guard's last-known-good stand-in.
func TestChange24hAnchorAgeIsBoundedOnEveryArm(t *testing.T) {
	pair := thinMarketTestPair(t)
	peg := pair.Quote
	usd, pegKey := usdQuoteAsset.String(), peg.String()
	weeks := 20 * 24 * time.Hour
	inside := timescale.Change24hAnchorTolerance - 2*time.Minute
	outside := timescale.Change24hAnchorTolerance + 2*time.Minute

	for name, tc := range map[string]struct {
		byQuote map[string]agedAnchor
		want    string // "" = ErrChange24hUnavailable
	}{
		"literal anchor weeks old":                 {byQuote: map[string]agedAnchor{usd: {age: weeks, vwap: "0.5"}}},
		"literal anchor just past the tolerance":   {byQuote: map[string]agedAnchor{usd: {age: outside, vwap: "0.5"}}},
		"literal and peg anchors both stale":       {byQuote: map[string]agedAnchor{usd: {age: weeks, vwap: "0.5"}, pegKey: {age: weeks, vwap: "0.7"}}},
		"stale literal falls through to fresh peg": {byQuote: map[string]agedAnchor{usd: {age: weeks, vwap: "0.5"}, pegKey: {age: inside, vwap: "0.7"}}, want: "0.7"},
		"literal anchor inside the tolerance":      {byQuote: map[string]agedAnchor{usd: {age: inside, vwap: "0.5"}}, want: "0.5"},
		"outlier replaced by a stale stand-in":     {byQuote: map[string]agedAnchor{usd: {age: time.Minute, vwap: "100.0", trailing: true, trailingAge: 3 * time.Hour}}},
		"outlier replaced by a fresh stand-in":     {byQuote: map[string]agedAnchor{usd: {age: time.Minute, vwap: "100.0", trailing: true, trailingAge: time.Minute}}, want: "1.0"},
	} {
		t.Run(name, func(t *testing.T) {
			r := storeChange24hReader{s: &agedAnchorStore{byQuote: tc.byQuote}, pegs: []canonical.Asset{peg}}
			got, err := r.USDPrice24hAgo(context.Background(), pair.Base)
			if tc.want == "" {
				if !errors.Is(err, v1.ErrChange24hUnavailable) {
					t.Fatalf("anchor = %q, %v; want ErrChange24hUnavailable for an anchor older than %s",
						got, err, timescale.Change24hAnchorTolerance)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("anchor = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
