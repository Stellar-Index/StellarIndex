package pricingguard

import (
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fuzzRows decodes "minutesAgo:vwap;minutesAgo:vwap;…" into trailing rows
// relative to mkRow's anchor. Malformed entries are dropped; exponent forms
// are skipped because Postgres NUMERIC text never carries one and a huge
// exponent only measures big.Rat's allocator.
func fuzzRows(spec string) []timescale.Vwap1mRow {
	var rows []timescale.Vwap1mRow
	for _, e := range strings.Split(spec, ";") {
		off, v, ok := strings.Cut(e, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(off)
		if err != nil || n < -10_000 || n > 10_000 || !fuzzVWAPText(v) {
			continue
		}
		rows = append(rows, mkRow(n, v))
		if len(rows) == 40 {
			break
		}
	}
	return rows
}

func fuzzVWAPText(s string) bool { return len(s) <= 64 && !strings.ContainsAny(s, "eE") }

// scaleRow multiplies a parseable VWAP by k exactly; unparseable text is
// left as-is so it stays unparseable.
func scaleRow(r timescale.Vwap1mRow, k *big.Rat) timescale.Vwap1mRow {
	if v, ok := new(big.Rat).SetString(r.VWAP); ok {
		r.VWAP = v.Mul(v, k).RatString()
	}
	return r
}

var selectGuardedSeeds = []struct{ cand, rows string }{
	{"1.0", "1:1.0;2:1.0;3:1.0;4:1.0;5:1.0;6:1.0;7:1.0;8:1.0;9:1.0;10:1.0"},
	{"100.0", "1:1.0;2:1.0;3:1.0;4:1.0;5:1.0;6:1.0;7:1.0;8:1.0;9:1.0;10:1.0"},
	{"0.001", "1:1.0;2:1.02;3:0.98;4:1.0;5:1.01"},
	{"50", "1:1;2:1.1"},
	{"1", ""},
	{"abc", "1:1.0;2:1.0"},
	{"100", "0:100;-1:100;1:1;2:1;3:1"},
	{"-5", "1:1;2:1;3:1;4:1;5:1;6:1"},
	{"7", "1:x;2:0;3:-1;4:7;5:7"},
	{"3/1", "1:1;2:1;3:1;4:1;5:1;6:1;7:1;8:1"},
}

// FuzzSelectGuardedVWAP1m pins the guard's selection against an
// independent reconstruction of its contract: the baseline is the strictly
// older rows (index-aligned), the decision is aggregate.GuardServedVWAP's,
// a served substitute is a parseable older row, rows at/after the
// candidate never influence the answer, and the decision is invariant
// under an exact positive rescaling of every price (the band is
// ratio/MAD-homogeneous — a unit change must not move it).
func FuzzSelectGuardedVWAP1m(f *testing.F) {
	for _, s := range selectGuardedSeeds {
		f.Add(s.cand, s.rows)
	}
	f.Fuzz(func(t *testing.T, cand, spec string) {
		if !fuzzVWAPText(cand) {
			return
		}
		candidate := mkRow(0, cand)
		rows := fuzzRows(spec)

		served, rejected, lowConf := selectGuardedVWAP1m(candidate, rows)
		if pub, pubRej := SelectGuardedVWAP1m(candidate, rows); pubRej != rejected || !reflect.DeepEqual(pub, served) {
			t.Fatalf("exported and internal selection disagree")
		}

		candRat, candOK := new(big.Rat).SetString(cand)
		if !candOK {
			if rejected || lowConf || !reflect.DeepEqual(served, candidate) {
				t.Fatalf("unparseable candidate %q must be served as-is (rejected=%v lowConf=%v)", cand, rejected, lowConf)
			}
			return
		}

		var older []timescale.Vwap1mRow
		var trailing []*big.Rat
		for _, r := range rows {
			if r.Bucket.Before(candidate.Bucket) {
				older = append(older, r)
				v, ok := new(big.Rat).SetString(r.VWAP)
				if !ok {
					v = nil
				}
				trailing = append(trailing, v)
			}
		}
		accept, idx := aggregate.GuardServedVWAP(candRat, trailing)
		if rejected == accept {
			t.Fatalf("rejected=%v but GuardServedVWAP accept=%v", rejected, accept)
		}
		if rejected {
			if lowConf {
				t.Fatal("a rejected candidate cannot be low-confidence")
			}
			if !reflect.DeepEqual(served, older[idx]) {
				t.Fatalf("served %+v, want older[%d]=%+v", served, idx, older[idx])
			}
			if !served.Bucket.Before(candidate.Bucket) {
				t.Fatalf("substitute bucket %v is not older than the candidate %v", served.Bucket, candidate.Bucket)
			}
			if _, ok := new(big.Rat).SetString(served.VWAP); !ok {
				t.Fatalf("substitute VWAP %q is unparseable", served.VWAP)
			}
		} else {
			if !reflect.DeepEqual(served, candidate) {
				t.Fatalf("accepted candidate must be served unchanged")
			}
			if lowConf != !aggregate.ServedBaselineValidated(trailing) {
				t.Fatalf("lowConfidence=%v disagrees with ServedBaselineValidated", lowConf)
			}
		}

		// Rows at or after the candidate bucket are not baseline.
		noisy := append(append([]timescale.Vwap1mRow{}, rows...), mkRow(0, "999999"), mkRow(-3, "0.000001"))
		s2, r2, l2 := selectGuardedVWAP1m(candidate, noisy)
		if r2 != rejected || l2 != lowConf || !reflect.DeepEqual(s2, served) {
			t.Fatalf("rows at/after the candidate changed the decision")
		}

		// Exact rescaling by 1000 (a unit change) must not move the decision.
		k := big.NewRat(1000, 1)
		scaled := make([]timescale.Vwap1mRow, len(rows))
		for i := range rows {
			scaled[i] = scaleRow(rows[i], k)
		}
		s3, r3, l3 := selectGuardedVWAP1m(scaleRow(candidate, k), scaled)
		if r3 != rejected || l3 != lowConf || !s3.Bucket.Equal(served.Bucket) {
			t.Fatalf("rescaling by 1000 changed the decision: rejected %v->%v lowConf %v->%v bucket %v->%v",
				rejected, r3, lowConf, l3, served.Bucket, s3.Bucket)
		}
	})
}

// FuzzSelectGuardedVWAP1mAt pins the point-in-time staleness contract: a
// non-rejected candidate is always served; a substitute is served iff its
// bucket CLOSE (start + 1m) is within maxStaleness of ts, inclusive.
func FuzzSelectGuardedVWAP1mAt(f *testing.F) {
	for _, s := range selectGuardedSeeds {
		f.Add(s.cand, s.rows, int64(0), int64(5*time.Minute))
		f.Add(s.cand, s.rows, int64(time.Hour), int64(time.Minute))
	}
	f.Fuzz(func(t *testing.T, cand, spec string, tsOffset, maxStale int64) {
		if !fuzzVWAPText(cand) {
			return
		}
		candidate := mkRow(0, cand)
		rows := fuzzRows(spec)
		ts := baseTS.Add(time.Duration(tsOffset))
		stale := time.Duration(maxStale)

		served, ok := SelectGuardedVWAP1mAt(candidate, rows, ts, stale)
		ref, rejected, lowConfidence := selectGuardedVWAP1m(candidate, rows)
		if !rejected {
			if lowConfidence {
				// An accept against an empty trailing baseline is an
				// unvalidated fail-open (W6-fresh-1): the series path
				// still serves it, but a point-in-time read must not.
				if ok {
					t.Fatalf("a low-confidence unvalidated accept must not be served point-in-time (ok=%v)", ok)
				}
				return
			}
			if !ok || !reflect.DeepEqual(served, candidate) {
				t.Fatalf("a non-rejected candidate must be served unchanged (ok=%v)", ok)
			}
			return
		}
		closeAt := ref.Bucket.Add(time.Minute)
		if want := ts.Sub(closeAt) <= stale; ok != want {
			t.Fatalf("ok=%v, want %v (ts-close=%v, maxStaleness=%v)", ok, want, ts.Sub(closeAt), stale)
		}
		if ok && !reflect.DeepEqual(served, ref) {
			t.Fatalf("served %+v, want the last-known-good %+v", served, ref)
		}
		if !ok && !reflect.DeepEqual(served, timescale.Vwap1mRow{}) {
			t.Fatalf("withheld answer carried a row: %+v", served)
		}
	})
}

// FuzzSubstanceOK checks the floor against an exact big.Int/big.Rat
// reference and its monotonicity: more volume, buckets or span can never
// turn a pass into a withhold. Span is a DB-measured interval inside the
// policy window, so it is held to a non-negative range well short of
// time.Duration's overflow.
func FuzzSubstanceOK(f *testing.F) {
	f.Add("1000", int64(20), int64(6*3600), "1000", int64(20), int64(6*3600))
	f.Add("999.9999999", int64(20), int64(6*3600), "1000", int64(20), int64(6*3600))
	f.Add("1000", int64(19), int64(6*3600), "1000", int64(20), int64(6*3600))
	f.Add("1000", int64(20), int64(6*3600-1), "1000", int64(20), int64(6*3600))
	f.Add("x", int64(100), int64(1<<20), "0.5", int64(1), int64(1))
	f.Add("123456789012345678901234567890.1", int64(1), int64(1), "123456789012345678901234567890", int64(0), int64(0))
	f.Fuzz(func(t *testing.T, vol string, buckets, span int64, minVol string, minBuckets, minSpanSec int64) {
		if !fuzzVWAPText(vol) || !fuzzVWAPText(minVol) {
			return
		}
		if span < 0 || span > 1<<33 || minSpanSec < 0 || minSpanSec > 1<<33 {
			return
		}
		floor, ok := new(big.Rat).SetString(minVol)
		if !ok {
			return
		}
		policy := SubstancePolicy{MinVolumeUSD: floor, MinBuckets: minBuckets, MinSpan: time.Duration(minSpanSec) * time.Second}
		v, vOK := new(big.Rat).SetString(vol)
		var volArg *big.Rat
		if vOK {
			volArg = v
		}
		got := SubstanceOK(volArg, buckets, span, policy)

		refVol := new(big.Rat)
		if vOK {
			refVol = v
		}
		spanNS := new(big.Int).Mul(big.NewInt(span), big.NewInt(int64(time.Second)))
		want := refVol.Cmp(floor) >= 0 && buckets >= minBuckets && spanNS.Cmp(big.NewInt(int64(policy.MinSpan))) >= 0
		if got != want {
			t.Fatalf("SubstanceOK(%s, %d, %d) = %v, reference %v (policy vol=%s buckets=%d span=%v)",
				vol, buckets, span, got, want, floor.RatString(), minBuckets, policy.MinSpan)
		}
		if !got {
			return
		}
		more := new(big.Rat).Add(refVol, big.NewRat(1, 1))
		if !SubstanceOK(more, buckets, span, policy) {
			t.Fatal("adding volume turned a pass into a withhold")
		}
		if buckets < math.MaxInt64 && !SubstanceOK(volArg, buckets+1, span, policy) {
			t.Fatal("adding a bucket turned a pass into a withhold")
		}
		if !SubstanceOK(volArg, buckets, span+1, policy) {
			t.Fatal("widening the span turned a pass into a withhold")
		}
	})
}

// FuzzSubstancePolicyFromValues: whatever the config holds, the defaulted
// policy's volume floor is strictly positive (never zero, never nil), and a
// finite positive configured floor is carried exactly — no float rounding
// on its way into the rational compare.
func FuzzSubstancePolicyFromValues(f *testing.F) {
	f.Add(1000.0, 20, 360, 24)
	f.Add(0.0, 0, 0, 0)
	f.Add(-1.0, -5, -5, -5)
	f.Add(math.Inf(1), 1, 1, 1)
	f.Add(math.NaN(), 1, 1, 1)
	f.Add(5e-324, 1, 1, 1)
	f.Fuzz(func(t *testing.T, vol float64, b, spanMin, winH int) {
		p := SubstancePolicyFromValues(vol, b, spanMin, winH).withDefaults()
		if p.MinVolumeUSD == nil || p.MinVolumeUSD.Sign() <= 0 {
			t.Fatalf("volume floor %v is not strictly positive for config %v", p.MinVolumeUSD, vol)
		}
		if vol > 0 && !math.IsInf(vol, 0) {
			if want := new(big.Rat).SetFloat64(vol); p.MinVolumeUSD.Cmp(want) != 0 {
				t.Fatalf("floor %s, want exactly %s", p.MinVolumeUSD.RatString(), want.RatString())
			}
		} else if p.MinVolumeUSD.Cmp(big.NewRat(DefaultSubstanceMinVolumeUSD, 1)) != 0 {
			t.Fatalf("non-positive/non-finite config %v must fall back to the default floor, got %s", vol, p.MinVolumeUSD.RatString())
		}
		if b == 0 && p.MinBuckets != DefaultSubstanceMinBuckets {
			t.Fatalf("zero buckets must default, got %d", p.MinBuckets)
		}
		if b != 0 && p.MinBuckets != int64(b) {
			t.Fatalf("buckets %d not carried, got %d", b, p.MinBuckets)
		}
	})
}

// FuzzPairCacheKey: the verdict key is direction-insensitive and does not
// alias two different pairs.
func FuzzPairCacheKey(f *testing.F) {
	f.Add("native", "fiat:USD", "crypto:XLM", "fiat:USD")
	f.Add("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "native", "native", "fiat:EUR")
	f.Add("fiat:EUR", "fiat:USD", "fiat:USD", "fiat:EUR")
	f.Fuzz(func(t *testing.T, a, b, c, d string) {
		as := make([]canonical.Asset, 0, 4)
		for _, s := range []string{a, b, c, d} {
			x, err := canonical.ParseAsset(s)
			if err != nil {
				return
			}
			as = append(as, x)
		}
		k1, k1r := pairCacheKey(as[0], as[1]), pairCacheKey(as[1], as[0])
		if k1 != k1r {
			t.Fatalf("key is direction-sensitive: %q vs %q", k1, k1r)
		}
		same := func(x, y canonical.Asset) bool { return x.String() == y.String() }
		setEq := (same(as[0], as[2]) && same(as[1], as[3])) || (same(as[0], as[3]) && same(as[1], as[2]))
		if (k1 == pairCacheKey(as[2], as[3])) != setEq {
			t.Fatalf("key aliasing: {%s,%s} vs {%s,%s}", as[0], as[1], as[2], as[3])
		}
	})
}

// FuzzIsDirectoryScamFlagged: every scam-class tag is recognised whatever
// its case and surrounding whitespace, among any other tags, in any
// position; a tag set with no scam-class member is never flagged.
func FuzzIsDirectoryScamFlagged(f *testing.F) {
	f.Add("verified,exchange", uint8(0), uint8(0), true)
	f.Add("", uint8(1), uint8(3), false)
	f.Add(" x ", uint8(2), uint8(255), true)
	f.Fuzz(func(t *testing.T, others string, which, caseMask uint8, inject bool) {
		tags := strings.Split(others, ",")
		base := false
		for _, tg := range tags {
			for _, s := range timescale.DirectoryScamFlagTags {
				if strings.EqualFold(strings.TrimSpace(tg), s) {
					base = true
				}
			}
		}
		if got := IsDirectoryScamFlagged(tags); got != base {
			t.Fatalf("IsDirectoryScamFlagged(%q) = %v, want %v", tags, got, base)
		}
		if !inject || len(timescale.DirectoryScamFlagTags) == 0 {
			return
		}
		tag := []rune(timescale.DirectoryScamFlagTags[int(which)%len(timescale.DirectoryScamFlagTags)])
		for i := range tag {
			if caseMask&(1<<(i%8)) != 0 {
				tag[i] = []rune(strings.ToUpper(string(tag[i])))[0]
			}
		}
		mangled := " \t" + string(tag) + "\n "
		pos := int(which) % (len(tags) + 1)
		withTag := append(append(append([]string{}, tags[:pos]...), mangled), tags[pos:]...)
		if !IsDirectoryScamFlagged(withTag) {
			t.Fatalf("scam tag %q not recognised in %q", mangled, withTag)
		}
	})
}
