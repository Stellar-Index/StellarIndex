package aggregate

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

type twapBucketCase struct {
	name        string
	buckets     []ClosedBucket
	start, end  time.Time
	noCarry     bool   // maxCarry 0; otherwise 15 minutes
	want        string // exact a/b; "" means no value
	wantBuckets int
	wantNewest  time.Time
	wantErr     error
}

func TestTWAPFromBuckets(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	b := func(start time.Time, vwap string) ClosedBucket { return ClosedBucket{Start: start, VWAP: vwap} }
	huge := "170141183460469231731687303715884105727.0000001" // above 2^126, seven decimals

	cases := []twapBucketCase{
		{
			name: "one bucket inside", buckets: []ClosedBucket{b(at(10), "2.5")}, start: at(0), end: at(30),
			want: "5/2", wantBuckets: 1, wantNewest: at(10),
		},
		{
			name: "carry-in only", buckets: []ClosedBucket{b(at(-5), "3")}, start: at(0), end: at(30),
			want: "3", wantBuckets: 1, wantNewest: at(-5),
		},
		// Only the newest pre-window bucket carries in; the older one is superseded before start.
		{
			name: "older pre-window bucket has no weight", buckets: []ClosedBucket{b(at(-9), "100"), b(at(-5), "3")}, start: at(0), end: at(30),
			want: "3", wantBuckets: 1, wantNewest: at(-5),
		},
		// 1 holds 0..10 (carry-in), 2 holds 10..30 across an idle gap: (1*600 + 2*1200)/1800.
		{
			name: "idle gap carries the last price", buckets: []ClosedBucket{b(at(-1), "1"), b(at(10), "2")}, start: at(0), end: at(30),
			want: "5/3", wantBuckets: 2, wantNewest: at(10),
		},
		// A bucket starting exactly at start holds from start (the carry-in gets 0); one starting at end covers nothing in the window.
		{
			name: "starts on both edges", buckets: []ClosedBucket{b(at(-1), "9"), b(at(0), "4"), b(at(30), "1000")}, start: at(0), end: at(30),
			want: "4", wantBuckets: 1, wantNewest: at(0),
		},
		// The 12:29 bucket covers [12:29, 12:30), the window's last minute: 60 s at 1 and 60 s at 1000.
		{
			name: "last minute carries its own weight", buckets: []ClosedBucket{b(at(28), "1"), b(at(29), "1000")}, start: at(0), end: at(30),
			want: "1001/2", wantBuckets: 2, wantNewest: at(29),
		},
		{
			name: "bucket after end ignored", buckets: []ClosedBucket{b(at(5), "6"), b(at(31), "1000")}, start: at(0), end: at(30),
			want: "6", wantBuckets: 1, wantNewest: at(5),
		},
		{
			name: "unsorted input is sorted", buckets: []ClosedBucket{b(at(20), "4"), b(at(-1), "1"), b(at(10), "2")}, start: at(0), end: at(30),
			want: "7/3", wantBuckets: 3, wantNewest: at(20),
		},
		{
			name: "extreme values stay exact", buckets: []ClosedBucket{b(at(-1), huge), b(at(15), "0.0000001")}, start: at(0), end: at(30),
			want: new(big.Rat).Add(
				new(big.Rat).Quo(mustRat(t, huge), big.NewRat(2, 1)),
				big.NewRat(1, 20_000_000)).RatString(),
			wantBuckets: 2, wantNewest: at(15),
		},
		{
			name: "carry-in at the age limit counts", buckets: []ClosedBucket{b(at(-15), "3")}, start: at(0), end: at(30),
			want: "3", wantBuckets: 1, wantNewest: at(-15),
		},
		{name: "stale carry-in is no value", buckets: []ClosedBucket{b(at(-30*24*60), "1")}, start: at(0), end: at(30)},
		{
			name: "no carry-in when maxCarry is zero", buckets: []ClosedBucket{b(at(-1), "5"), b(at(10), "2")}, start: at(0), end: at(30),
			noCarry: true, want: "2", wantBuckets: 1, wantNewest: at(10),
		},
		// Exact nanosecond weights: 1 holds 0.9..1.1 s, 100 holds 1.1..2 s: (0.2*1 + 0.9*100)/1.1 = 82; floored Unix seconds
		// would weigh each 1 s and give 101/2.
		{
			name: "sub-second weights are exact", buckets: []ClosedBucket{b(ms(900), "1"), b(ms(1100), "100")}, start: ms(0), end: ms(2000),
			want: "82", wantBuckets: 2, wantNewest: ms(1100),
		},
		{
			name: "sub-second window covered by carry-in", buckets: []ClosedBucket{b(ms(-10_000), "3")}, start: ms(0), end: ms(500),
			want: "3", wantBuckets: 1, wantNewest: ms(-10_000),
		},
		{name: "no bucket covers the window", buckets: []ClosedBucket{b(at(30), "1"), b(at(40), "2")}, start: at(0), end: at(30)},
		{name: "empty input", start: at(0), end: at(30)},
		{
			name: "duplicate start rejected", buckets: []ClosedBucket{b(at(5), "1"), b(at(5), "1")}, start: at(0), end: at(30),
			wantErr: ErrDuplicateBucketStart,
		},
		{name: "empty window rejected", buckets: []ClosedBucket{b(at(5), "1")}, start: at(30), end: at(30), wantErr: ErrInvalidTWAPWindow},
	}
	for _, bad := range []string{"0", "0.000", "NaN", "1/3", "0x10", "010/1", "1_000", "+5", "-1", "1e3", " 1", "1 ", "1\n", "", ".5", "5."} {
		cases = append(cases, twapBucketCase{name: "VWAP " + bad + " rejected", buckets: []ClosedBucket{b(at(5), bad)}, start: at(0), end: at(30), wantErr: ErrInvalidBucketVWAP})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maxCarry := 15 * time.Minute
			if tc.noCarry {
				maxCarry = 0
			}
			res, ok, err := TWAPFromBuckets(tc.buckets, tc.start, tc.end, maxCarry)
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
