package v1

import (
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

var schedBase = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func sec(n int) time.Time { return schedBase.Add(time.Duration(n) * time.Second) }

func seg(start int, eps int64, exp int) emissionSegment {
	return emissionSegment{start: sec(start), eps: big.NewInt(eps), exp: sec(exp)}
}

// bruteScheduled sums second by second: the rate in force at s is the last update starting at or before s, until its expiration.
func bruteScheduled(tl []emissionSegment, from, to int) *big.Rat {
	sum := new(big.Int)
	for s := from; s < to; s++ {
		at := sec(s)
		var cur *emissionSegment
		for i := range tl {
			if !tl[i].start.After(at) {
				cur = &tl[i]
			}
		}
		if cur != nil && at.Before(cur.exp) {
			sum.Add(sum, cur.eps)
		}
	}
	return new(big.Rat).SetFrac(sum, big.NewInt(10_000_000))
}

func randomTimeline(r *rand.Rand) []emissionSegment {
	n := r.IntN(5)
	starts := make([]int, n)
	for i := range starts {
		starts[i] = r.IntN(400)
	}
	slices.Sort(starts)
	out := make([]emissionSegment, n)
	for i, st := range starts {
		// Rates up to 2^62 so a window's product passes 2^63; expirations before, at and after the next update.
		out[i] = seg(st, r.Int64N(1<<62)+1, st+r.IntN(300)-20)
	}
	return out
}

// TestScheduledTokens_MatchesPerSecondSum: exact against an independent second-by-second sum, including expirations,
// replacements, same-second updates and expirations before the start.
func TestScheduledTokens_MatchesPerSecondSum(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for i := range 400 {
		tl := randomTimeline(r)
		from := r.IntN(450) - 20
		to := from + r.IntN(300)
		got, want := scheduledTokens(tl, sec(from), sec(to)), bruteScheduled(tl, from, to)
		if got.Cmp(want) != 0 {
			t.Fatalf("case %d: timeline %+v [%d,%d) = %s, want %s", i, tl, from, to, got.RatString(), want.RatString())
		}
	}
}

// TestScheduledTokens_Additive: buckets sum to the window, and splitting the window anywhere changes nothing.
func TestScheduledTokens_Additive(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	for i := range 300 {
		tl := randomTimeline(r)
		from, to := r.IntN(100), 100+r.IntN(400)
		whole := scheduledTokens(tl, sec(from), sec(to))
		cut := from + r.IntN(to-from+1)
		split := new(big.Rat).Add(scheduledTokens(tl, sec(from), sec(cut)), scheduledTokens(tl, sec(cut), sec(to)))
		if split.Cmp(whole) != 0 {
			t.Fatalf("case %d: split at %d = %s, whole %s", i, cut, split.RatString(), whole.RatString())
		}
		buckets := new(big.Rat)
		for b := from; b < to; b += 37 {
			buckets.Add(buckets, scheduledTokens(tl, sec(b), sec(min(b+37, to))))
		}
		if buckets.Cmp(whole) != 0 {
			t.Fatalf("case %d: buckets = %s, whole %s", i, buckets.RatString(), whole.RatString())
		}
	}
}

// TestScheduledBuckets_MatchesPerBucketSum: the one-sweep bucketing equals scheduledTokens bucket by bucket, including a
// short last bucket.
func TestScheduledBuckets_MatchesPerBucketSum(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 17))
	for i := range 300 {
		tl := randomTimeline(r)
		from, end, width := r.IntN(100)-20, 100+r.IntN(400), 1+r.IntN(60)
		got := scheduledBuckets(tl, sec(from), sec(end), time.Duration(width)*time.Second)
		n := 0
		for b := from; b < end; b += width {
			want := scheduledTokens(tl, sec(b), sec(min(b+width, end)))
			if n >= len(got) || got[n].Cmp(want) != 0 {
				t.Fatalf("case %d bucket %d: got %v, want %s", i, n, got, want.RatString())
			}
			n++
		}
		if n != len(got) {
			t.Fatalf("case %d: %d buckets, want %d", i, len(got), n)
		}
	}
}

// BenchmarkScheduledBuckets is the widest request: 1,998 daily buckets over 200 updates.
func BenchmarkScheduledBuckets(b *testing.B) {
	tl := make([]emissionSegment, 200)
	for i := range tl {
		tl[i] = seg(i*864_000, int64(i+1)*1_000_000_007, i*864_000+900_000)
	}
	for b.Loop() {
		scheduledBuckets(tl, sec(0), sec(1998*86_400), 24*time.Hour)
	}
}

// TestScheduledTokens_ExpiredAndReplaced: an expired stream contributes nothing, and a later update replaces the rate from
// its own timestamp even while the earlier one has not expired.
func TestScheduledTokens_ExpiredAndReplaced(t *testing.T) {
	expired := []emissionSegment{seg(0, 10_000_000, 100)}
	if got := scheduledTokens(expired, sec(100), sec(1000)); got.Sign() != 0 {
		t.Fatalf("expired stream = %s, want 0", got.RatString())
	}
	if got := scheduledTokens(expired, sec(50), sec(1000)); got.Cmp(big.NewRat(50, 1)) != 0 {
		t.Fatalf("stream expiring inside = %s, want 50 (clamped at expiration)", got.RatString())
	}
	replaced := []emissionSegment{seg(0, 10_000_000, 1000), seg(10, 30_000_000, 20)}
	// 10s at 1/s, then 10s at 3/s, then nothing: the replacement's expiration ends the stream, not the first's.
	if got := scheduledTokens(replaced, sec(0), sec(1000)); got.Cmp(big.NewRat(40, 1)) != 0 {
		t.Fatalf("replaced = %s, want 40", got.RatString())
	}
	// The rate in force at the window's start comes from an update before it.
	if got := scheduledTokens([]emissionSegment{seg(-3600, 1, 7200)}, sec(0), sec(3600)); scheduledTokensString(got) != "0.00036" {
		t.Fatalf("seeded = %s, want 0.00036", scheduledTokensString(got))
	}
}

// TestScheduledTokensString_SevenDigitsExact: eps at 10^7 over whole seconds renders with at most 7 fractional digits,
// exact above 2^63.
func TestScheduledTokensString_SevenDigitsExact(t *testing.T) {
	tl := []emissionSegment{{start: sec(0), eps: new(big.Int).SetUint64(18_446_744_073_709_551_615), exp: sec(10)}}
	if got := scheduledTokensString(scheduledTokens(tl, sec(0), sec(3))); got != "5534023222112.8654845" {
		t.Fatalf("got %s", got)
	}
}
