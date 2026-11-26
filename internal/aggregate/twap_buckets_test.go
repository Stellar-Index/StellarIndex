package aggregate

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestTWAPFromBuckets(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	b := func(m int, vwap string) ClosedBucket { return ClosedBucket{Close: at(m), VWAP: vwap} }
	huge := "170141183460469231731687303715884105727.0000001" // above 2^126, seven decimals

	cases := []struct {
		name        string
		buckets     []ClosedBucket
		start, end  time.Time
		want        string // exact a/b; "" means no value
		wantBuckets int
		wantNewest  time.Time
		wantErr     error
	}{
		{
			name: "one bucket inside", buckets: []ClosedBucket{b(10, "2.5")}, start: at(0), end: at(30),
			want: "5/2", wantBuckets: 1, wantNewest: at(10),
		},
		{
			name: "carry-in only", buckets: []ClosedBucket{b(-5, "3")}, start: at(0), end: at(30),
			want: "3", wantBuckets: 1, wantNewest: at(-5),
		},
		// Only the newest pre-window bucket carries in; the older one is fully superseded before start.
		{
			name: "older pre-window bucket has no weight", buckets: []ClosedBucket{b(-9, "100"), b(-5, "3")}, start: at(0), end: at(30),
			want: "3", wantBuckets: 1, wantNewest: at(-5),
		},
		// 1 holds 0..10 (carry-in), 2 holds 10..30 across an idle gap: (1*600 + 2*1200)/1800.
		{
			name: "idle gap carries the last price", buckets: []ClosedBucket{b(-1, "1"), b(10, "2")}, start: at(0), end: at(30),
			want: "5/3", wantBuckets: 2, wantNewest: at(10),
		},
		// A close exactly at start holds from start (the carry-in gets 0s); a close exactly at end holds for 0s.
		{
			name: "closes on both edges", buckets: []ClosedBucket{b(-1, "9"), b(0, "4"), b(30, "1000")}, start: at(0), end: at(30),
			want: "4", wantBuckets: 1, wantNewest: at(0),
		},
		{
			name: "bucket after end ignored", buckets: []ClosedBucket{b(5, "6"), b(31, "1000")}, start: at(0), end: at(30),
			want: "6", wantBuckets: 1, wantNewest: at(5),
		},
		{
			name: "unsorted input is sorted", buckets: []ClosedBucket{b(20, "4"), b(-1, "1"), b(10, "2")}, start: at(0), end: at(30),
			want: "7/3", wantBuckets: 3, wantNewest: at(20),
		},
		{
			name: "extreme values stay exact", buckets: []ClosedBucket{b(-1, huge), b(15, "0.0000001")}, start: at(0), end: at(30),
			want: new(big.Rat).Add(
				new(big.Rat).Quo(mustRat(t, huge), big.NewRat(2, 1)),
				big.NewRat(1, 20_000_000)).RatString(),
			wantBuckets: 2, wantNewest: at(15),
		},
		{name: "no bucket covers the window", buckets: []ClosedBucket{b(30, "1"), b(40, "2")}, start: at(0), end: at(30)},
		{name: "empty input", start: at(0), end: at(30)},
		{
			name: "duplicate close rejected", buckets: []ClosedBucket{b(5, "1"), b(5, "1")}, start: at(0), end: at(30),
			wantErr: ErrDuplicateBucketClose,
		},
		{name: "zero VWAP rejected", buckets: []ClosedBucket{b(5, "0")}, start: at(0), end: at(30), wantErr: ErrInvalidBucketVWAP},
		{name: "garbage VWAP rejected", buckets: []ClosedBucket{b(5, "NaN")}, start: at(0), end: at(30), wantErr: ErrInvalidBucketVWAP},
		{name: "empty window rejected", buckets: []ClosedBucket{b(5, "1")}, start: at(30), end: at(30), wantErr: ErrInvalidTWAPWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok, err := TWAPFromBuckets(tc.buckets, tc.start, tc.end)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil || tc.want == "" {
				if ok || res.Price != nil {
					t.Fatalf("got a value %v (ok=%v), want no value", res.Price, ok)
				}
				return
			}
			if !ok || res.Price.RatString() != tc.want {
				t.Fatalf("price = %v (ok=%v), want %s", res.Price, ok, tc.want)
			}
			if res.Buckets != tc.wantBuckets || !res.Newest.Equal(tc.wantNewest) {
				t.Fatalf("buckets=%d newest=%s, want %d %s", res.Buckets, res.Newest, tc.wantBuckets, tc.wantNewest)
			}
		})
	}
}

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("parse %q", s)
	}
	return r
}
