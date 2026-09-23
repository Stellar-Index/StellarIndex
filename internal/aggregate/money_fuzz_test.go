package aggregate

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Property fuzz targets for the exact-rational money path. Every
// reference below is computed independently of the function under test
// (different association, different primitive, or an order statistic
// instead of a sort), so a target fails on a wrong value, not only on
// a panic.

// fuzzBytes is a cursor over fuzz input that decodes arbitrary-width
// signed integers, so amounts routinely exceed 2^63 and 2^128.
type fuzzBytes struct{ b []byte }

func (r *fuzzBytes) next() (byte, bool) {
	if len(r.b) == 0 {
		return 0, false
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c, true
}

// bigInt reads a length byte (low 5 bits = 1..32 magnitude bytes, bit 7
// = negative, bit 6 = force zero) then the magnitude.
func (r *fuzzBytes) bigInt() (*big.Int, bool) {
	h, ok := r.next()
	if !ok {
		return nil, false
	}
	n := int(h&0x1f) + 1
	if n > len(r.b) {
		n = len(r.b)
	}
	v := new(big.Int).SetBytes(r.b[:n])
	r.b = r.b[n:]
	if h&0x40 != 0 {
		v.SetInt64(0)
	}
	if h&0x80 != 0 {
		v.Neg(v)
	}
	return v, true
}

// posRat reads a strictly positive rational (num, den forced >= 1).
func (r *fuzzBytes) posRat() (*big.Rat, bool) {
	num, ok := r.bigInt()
	if !ok {
		return nil, false
	}
	den, ok := r.bigInt()
	if !ok {
		return nil, false
	}
	num.Abs(num).Add(num, big.NewInt(1))
	den.Abs(den).Add(den, big.NewInt(1))
	return new(big.Rat).SetFrac(num, den), true
}

const fuzzMaxItems = 48

var fuzzT0 = time.Unix(1_700_000_000, 0).UTC()

// fuzzTrades decodes up to fuzzMaxItems trades, each with a source in
// {a,b,c}, a strictly increasing timestamp, and possibly non-positive
// amounts (the functions under test must skip those).
func fuzzTrades(data []byte) []canonical.Trade {
	r := &fuzzBytes{b: data}
	var out []canonical.Trade
	ts := fuzzT0
	for len(out) < fuzzMaxItems {
		meta, ok := r.next()
		if !ok {
			break
		}
		b, ok1 := r.bigInt()
		q, ok2 := r.bigInt()
		if !ok1 || !ok2 {
			break
		}
		ts = ts.Add(time.Duration(meta>>2)*time.Second + time.Nanosecond)
		out = append(out, canonical.Trade{
			Source:      string(rune('a' + int(meta&3)%3)),
			Ledger:      uint32(len(out) + 1),
			TxHash:      fmt.Sprintf("%064x", len(out)),
			Timestamp:   ts,
			BaseAmount:  canonical.NewAmount(b),
			QuoteAmount: canonical.NewAmount(q),
		})
	}
	return out
}

func validPrice(t canonical.Trade) (*big.Rat, bool) {
	b, q := t.BaseAmount.BigInt(), t.QuoteAmount.BigInt()
	if b.Sign() <= 0 || q.Sign() <= 0 {
		return nil, false
	}
	return new(big.Rat).SetFrac(q, b), true
}

func cloneTrades(in []canonical.Trade) []canonical.Trade {
	out := make([]canonical.Trade, len(in))
	for i, t := range in {
		t.BaseAmount = canonical.NewAmount(t.BaseAmount.BigInt())
		t.QuoteAmount = canonical.NewAmount(t.QuoteAmount.BigInt())
		out[i] = t
	}
	return out
}

func sameTrades(a, b []canonical.Trade) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TxHash != b[i].TxHash || a[i].Source != b[i].Source ||
			!a[i].BaseAmount.Equal(b[i].BaseAmount) || !a[i].QuoteAmount.Equal(b[i].QuoteAmount) {
			return false
		}
	}
	return true
}

// isSubsequence reports whether sub appears in full in order (by TxHash,
// which fuzzTrades makes unique).
func isSubsequence(sub, full []canonical.Trade) bool {
	j := 0
	for i := range sub {
		for j < len(full) && full[j].TxHash != sub[i].TxHash {
			j++
		}
		if j == len(full) {
			return false
		}
		j++
	}
	return true
}

// seedTradeBytes encodes (base, quote) pairs in the fuzzTrades format.
func seedTradeBytes(pairs ...int64) []byte {
	var out []byte
	enc := func(v int64) {
		h := byte(7) // 8 magnitude bytes
		if v < 0 {
			h |= 0x80
			v = -v
		}
		out = append(out, h)
		var buf [8]byte
		big.NewInt(v).FillBytes(buf[:])
		out = append(out, buf[:]...)
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, byte(i*4+i/2))
		enc(pairs[i])
		enc(pairs[i+1])
	}
	return out
}

func addTradeSeeds(f *testing.F) {
	f.Add([]byte{})
	f.Add(seedTradeBytes(100, 200))
	f.Add(seedTradeBytes(100, 200, 100, 300, 0, 5, 100, -1))
	f.Add(seedTradeBytes(100, 200, 100, 0, 7, 9))
	// M5 masking case and the MNY-22 zero-MAD case.
	f.Add(seedTradeBytes(1, 100, 1, 100, 1, 100, 1, 100, 1, 200))
	f.Add(seedTradeBytes(100, 10000, 100, 10000, 100, 10000, 100, 10001))
	// A crash print and a fat-finger print against a tight cluster.
	f.Add(seedTradeBytes(10, 1000, 10, 1010, 10, 990, 10, 1, 10, 100000))
	// Amounts above 2^63 (Soroban i128 volumes).
	big1 := make([]byte, 0, 64)
	big1 = append(big1, 0, 0x1f)
	for i := 0; i < 32; i++ {
		big1 = append(big1, 0xff)
	}
	big1 = append(big1, 0x0f, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
	f.Add(big1)
}

// FuzzVWAP pins VWAP to an independent per-trade weighted mean and the
// volume helpers to unfiltered big.Int sums (no int64/float64 anywhere).
func FuzzVWAP(f *testing.F) {
	addTradeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		trades := fuzzTrades(data)
		orig := cloneTrades(trades)

		weighted := new(big.Rat)
		sumBase := new(big.Int)
		allBase, allQuote := new(big.Int), new(big.Int)
		var lo, hi *big.Rat
		valid := 0
		for _, tr := range trades {
			allBase.Add(allBase, tr.BaseAmount.BigInt())
			allQuote.Add(allQuote, tr.QuoteAmount.BigInt())
			p, ok := validPrice(tr)
			if !ok {
				continue
			}
			valid++
			b := tr.BaseAmount.BigInt()
			weighted.Add(weighted, new(big.Rat).Mul(p, new(big.Rat).SetInt(b)))
			sumBase.Add(sumBase, b)
			if lo == nil || p.Cmp(lo) < 0 {
				lo = p
			}
			if hi == nil || p.Cmp(hi) > 0 {
				hi = p
			}
		}

		got, err := VWAP(trades)
		if valid == 0 {
			if !errors.Is(err, ErrNoTrades) || got != nil {
				t.Fatalf("VWAP with no valid trade = (%v, %v), want ErrNoTrades", got, err)
			}
		} else {
			if err != nil {
				t.Fatalf("VWAP: %v", err)
			}
			want := weighted.Quo(weighted, new(big.Rat).SetInt(sumBase))
			if got.Cmp(want) != 0 {
				t.Fatalf("VWAP = %s, want %s", got.RatString(), want.RatString())
			}
			if got.Cmp(lo) < 0 || got.Cmp(hi) > 0 {
				t.Fatalf("VWAP %s outside [%s, %s]", got.RatString(), lo.RatString(), hi.RatString())
			}
		}
		if v := TotalBaseVolume(trades).BigInt(); v.Cmp(allBase) != 0 {
			t.Fatalf("TotalBaseVolume = %s, want %s", v, allBase)
		}
		if v := TotalQuoteVolume(trades).BigInt(); v.Cmp(allQuote) != 0 {
			t.Fatalf("TotalQuoteVolume = %s, want %s", v, allQuote)
		}

		contribs := SourceContributions(trades)
		if valid == 0 {
			if contribs != nil {
				t.Fatalf("SourceContributions with no valid trade = %v, want nil", contribs)
			}
		} else {
			cb, cq := new(big.Int), new(big.Int)
			count := 0
			for _, c := range contribs {
				if c.Weight < 0 || c.Weight > 1 {
					t.Fatalf("weight %v outside [0,1]", c.Weight)
				}
				cb.Add(cb, c.BaseVolume)
				cq.Add(cq, c.QuoteVolume)
				count += c.TradeCount
			}
			if count != valid || cb.Cmp(sumBase) != 0 {
				t.Fatalf("contributions count=%d base=%s, want %d / %s", count, cb, valid, sumBase)
			}
			if got != nil && new(big.Rat).SetFrac(cq, cb).Cmp(got) != 0 {
				t.Fatalf("contributions imply VWAP %s, want %s", new(big.Rat).SetFrac(cq, cb).RatString(), got.RatString())
			}
		}
		if !sameTrades(trades, orig) {
			t.Fatal("input trades mutated")
		}
	})
}

// FuzzComputeOHLC checks the bar invariants against the valid prints.
func FuzzComputeOHLC(f *testing.F) {
	addTradeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		trades := fuzzTrades(data)
		var prices []*big.Rat
		sumB, sumQ := new(big.Int), new(big.Int)
		for _, tr := range trades {
			if p, ok := validPrice(tr); ok {
				prices = append(prices, p)
				sumB.Add(sumB, tr.BaseAmount.BigInt())
				sumQ.Add(sumQ, tr.QuoteAmount.BigInt())
			}
		}
		bar, err := ComputeOHLC(trades)
		if len(prices) == 0 {
			if !errors.Is(err, ErrNoTrades) {
				t.Fatalf("ComputeOHLC with no valid trade: err=%v", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("ComputeOHLC: %v", err)
		}
		lo, hi := prices[0], prices[0]
		for _, p := range prices {
			if p.Cmp(lo) < 0 {
				lo = p
			}
			if p.Cmp(hi) > 0 {
				hi = p
			}
		}
		if bar.Open.Cmp(prices[0]) != 0 || bar.Close.Cmp(prices[len(prices)-1]) != 0 {
			t.Fatalf("open/close = %s/%s, want %s/%s", bar.Open.RatString(), bar.Close.RatString(),
				prices[0].RatString(), prices[len(prices)-1].RatString())
		}
		if bar.High.Cmp(hi) != 0 || bar.Low.Cmp(lo) != 0 {
			t.Fatalf("high/low = %s/%s, want %s/%s", bar.High.RatString(), bar.Low.RatString(), hi.RatString(), lo.RatString())
		}
		if bar.TradeCount != len(prices) {
			t.Fatalf("TradeCount = %d, want %d", bar.TradeCount, len(prices))
		}
		if bar.BaseVolume.BigInt().Cmp(sumB) != 0 || bar.QuoteVolume.BigInt().Cmp(sumQ) != 0 {
			t.Fatalf("volumes = %s/%s, want %s/%s", bar.BaseVolume, bar.QuoteVolume, sumB, sumQ)
		}
		// The bar must not alias the first price: mutating High must not move Open.
		if len(prices) > 1 && bar.Open.Cmp(prices[0]) != 0 {
			t.Fatal("open aliased to a later price")
		}
	})
}

// FuzzTWAP bounds TWAP by the prices that carried time weight; the
// fixed-point accumulator may truncate by at most 10^-40 relative.
func FuzzTWAP(f *testing.F) {
	addTradeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		trades := fuzzTrades(data)
		if len(trades) == 0 {
			if _, err := TWAP(trades, fuzzT0); !errors.Is(err, ErrNoTrades) {
				t.Fatalf("TWAP(empty) err = %v", err)
			}
			return
		}
		end := trades[len(trades)-1].Timestamp.Add(time.Minute)
		var lo, hi *big.Rat
		exact := new(big.Rat)
		total := new(big.Int)
		for i, tr := range trades {
			p, ok := validPrice(tr)
			if !ok {
				continue
			}
			next := end
			if i+1 < len(trades) {
				next = trades[i+1].Timestamp
			}
			d := big.NewInt(int64(next.Sub(tr.Timestamp)))
			exact.Add(exact, new(big.Rat).Mul(p, new(big.Rat).SetInt(d)))
			total.Add(total, d)
			if lo == nil || p.Cmp(lo) < 0 {
				lo = p
			}
			if hi == nil || p.Cmp(hi) > 0 {
				hi = p
			}
		}
		got, err := TWAP(trades, end)
		if lo == nil {
			if !errors.Is(err, ErrNoTrades) {
				t.Fatalf("TWAP with no valid trade err = %v", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("TWAP: %v", err)
		}
		want := exact.Quo(exact, new(big.Rat).SetInt(total))
		if got.Cmp(want) > 0 {
			t.Fatalf("TWAP %s exceeds exact %s (fixed point must truncate down)", got.RatString(), want.RatString())
		}
		// Each of n terms floors by < 1 unit of 1/(twapScale·ΣΔt), and
		// ΣΔt >= n ns, so the absolute error is below 1/twapScale.
		if new(big.Rat).Sub(want, got).Cmp(new(big.Rat).SetFrac(big.NewInt(1), twapScale)) >= 0 {
			t.Fatalf("TWAP %s too far below exact %s", got.RatString(), want.RatString())
		}
		if got.Cmp(hi) > 0 || got.Sign() < 0 {
			t.Fatalf("TWAP %s outside [0, %s]", got.RatString(), hi.RatString())
		}
	})
}

func fuzzRats(data []byte) []*big.Rat {
	r := &fuzzBytes{b: data}
	var out []*big.Rat
	for len(out) < fuzzMaxItems {
		v, ok := r.posRat()
		if !ok {
			break
		}
		out = append(out, v)
	}
	return out
}

func addRatSeeds(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 5, 0, 1})
	f.Add([]byte{0, 5, 0, 1, 0, 5, 0, 1, 0, 9, 0, 1})
	f.Add([]byte{0, 1, 0, 3, 0, 2, 0, 3, 0, 250, 0, 1, 0, 2, 0, 3})
	f.Add([]byte{0x1f, 1, 2, 3, 4, 5, 6, 7, 8, 9, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 0, 7, 0, 1, 0, 4, 0, 1})
}

// FuzzRobustStats pins medianRat / medianMemberRat / madRat to their
// order-statistic definitions and symmetricDev to its ratio-space
// contract.
func FuzzRobustStats(f *testing.F) {
	addRatSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		vals := fuzzRats(data)
		if len(vals) == 0 {
			if medianRat(nil) != nil || medianMemberRat(nil) != nil {
				t.Fatal("median of empty must be nil")
			}
			return
		}
		snap := make([]string, len(vals))
		for i, v := range vals {
			snap[i] = v.RatString()
		}
		n := len(vals)
		m := medianRat(vals)
		below, above := 0, 0
		for _, v := range vals {
			switch v.Cmp(m) {
			case -1:
				below++
			case 1:
				above++
			}
		}
		if 2*below > n || 2*above > n {
			t.Fatalf("median %s: %d below, %d above of %d", m.RatString(), below, above, n)
		}
		mm := medianMemberRat(vals)
		member := false
		for _, v := range vals {
			if v.Cmp(mm) == 0 {
				member = true
			}
		}
		if !member || mm.Cmp(m) > 0 || (n%2 == 1 && mm.Cmp(m) != 0) {
			t.Fatalf("medianMember %s vs median %s (member=%v, n=%d)", mm.RatString(), m.RatString(), member, n)
		}
		// Permutation invariance.
		rev := make([]*big.Rat, n)
		for i := range vals {
			rev[n-1-i] = vals[i]
		}
		if medianRat(rev).Cmp(m) != 0 || medianMemberRat(rev).Cmp(mm) != 0 {
			t.Fatal("median depends on input order")
		}
		// MAD: the median of |v − m|, checked the same way.
		mad := madRat(vals, m)
		if mad.Sign() < 0 {
			t.Fatalf("negative MAD %s", mad.RatString())
		}
		b2, a2 := 0, 0
		for _, v := range vals {
			d := new(big.Rat).Sub(v, m)
			d.Abs(d)
			switch d.Cmp(mad) {
			case -1:
				b2++
			case 1:
				a2++
			}
		}
		if 2*b2 > n || 2*a2 > n {
			t.Fatalf("MAD %s is not the median deviation", mad.RatString())
		}
		centre, scale := robustCentreScale(vals)
		if centre.Cmp(m) != 0 {
			t.Fatalf("robust centre %s, want %s", centre.RatString(), m.RatString())
		}
		wantScale := new(big.Rat).Mul(big.NewRat(7413, 5000), mad)
		if mad.Sign() == 0 {
			wantScale = new(big.Rat).Mul(big.NewRat(1, 200), m)
		}
		if scale.Cmp(wantScale) != 0 {
			t.Fatalf("scale %s, want %s", scale.RatString(), wantScale.RatString())
		}
		// A spread is sign-free: negating every value negates the centre
		// and leaves the scale unchanged.
		neg := make([]*big.Rat, n)
		for i, v := range vals {
			neg[i] = new(big.Rat).Neg(v)
		}
		if nc, ns := robustCentreScale(neg); nc.Cmp(new(big.Rat).Neg(m)) != 0 || ns.Cmp(scale) != 0 {
			t.Fatalf("negated input: centre %s scale %s, want %s / %s",
				nc.RatString(), ns.RatString(), new(big.Rat).Neg(m).RatString(), scale.RatString())
		}
		for i, v := range vals {
			if v.RatString() != snap[i] {
				t.Fatal("input values mutated")
			}
		}

		// symmetricDev over every value against the median.
		for _, p := range vals {
			dev := symmetricDev(p, m)
			add := new(big.Rat).Sub(p, m)
			add.Abs(add)
			if dev == nil || dev.Cmp(add) < 0 {
				t.Fatalf("symmetricDev(%s,%s)=%v below additive %s", p.RatString(), m.RatString(), dev, add.RatString())
			}
			// Mirror: dev(p) == dev(m²/p) — a ½× and a 2× are equally outlying.
			mirror := new(big.Rat).Mul(m, m)
			mirror.Quo(mirror, p)
			if md := symmetricDev(mirror, m); md.Cmp(dev) != 0 {
				t.Fatalf("symmetricDev not mirror-symmetric: dev(%s)=%s dev(%s)=%s",
					p.RatString(), dev.RatString(), mirror.RatString(), md.RatString())
			}
			if p.Cmp(m) >= 0 && dev.Cmp(add) != 0 {
				t.Fatalf("symmetricDev above centre must be additive: %s vs %s", dev.RatString(), add.RatString())
			}
		}
		if symmetricDev(new(big.Rat), m) != nil || symmetricDev(big.NewRat(-1, 1), m) != nil {
			t.Fatal("non-positive print against a positive centre must have no finite deviation")
		}
		// A non-positive centre has nothing to mirror around: plain |p − c|.
		for _, c := range []*big.Rat{new(big.Rat), new(big.Rat).Neg(m)} {
			for _, p := range []*big.Rat{vals[0], new(big.Rat).Neg(vals[0])} {
				add := new(big.Rat).Sub(p, c)
				add.Abs(add)
				if d := symmetricDev(p, c); d == nil || d.Cmp(add) != 0 {
					t.Fatalf("symmetricDev(%s, %s) = %v, want additive %s", p.RatString(), c.RatString(), d, add.RatString())
				}
			}
		}
	})
}

// FuzzFilterOutliers checks FilterOutliers is an order-preserving,
// price-scale-invariant, sigma-monotone subset filter.
func FuzzFilterOutliers(f *testing.F) {
	for _, s := range []byte{0, 1, 4, 8, 40} {
		f.Add(seedTradeBytes(1, 100, 1, 100, 1, 100, 1, 100, 1, 200), s, uint8(3))
		f.Add(seedTradeBytes(10, 1000, 10, 1010, 10, 990, 10, 1, 10, 100000), s, uint8(7))
		f.Add(seedTradeBytes(100, 10000, 100, 10000, 100, 10000, 100, 10001), s, uint8(2))
		// Two trades, one unusable; and two usable prices far apart.
		f.Add(seedTradeBytes(100, 200, 0, 5), s, uint8(1))
		f.Add(seedTradeBytes(1, 100, 1, 300, 0, 5), s, uint8(1))
		// Zero-quote prints that would drag the median if counted.
		f.Add(seedTradeBytes(1, 100, 1, 0, 1, 0, 1, 100, 1, 300), s, uint8(1))
		// Dispersed window where inverting the price moves the band.
		f.Add(seedTradeBytes(1, 100, 1, 101, 1, 104, 1, 115, 1, 131, 1, 60, 1, 70), s, uint8(5))
	}
	// Minimised input on which computing the price as base/quote instead
	// of quote/base changes the survivor set.
	f.Add([]byte("0 0'000000000 X'000000000!0000000000"), uint8(4), uint8(2))
	f.Fuzz(func(t *testing.T, data []byte, sigmaQ uint8, k uint8) {
		trades := fuzzTrades(data)
		orig := cloneTrades(trades)
		sigma := float64(sigmaQ) / 4
		got := FilterOutliers(trades, sigma)
		if !sameTrades(trades, orig) {
			t.Fatal("input trades mutated")
		}
		if !isSubsequence(got, trades) {
			t.Fatal("output is not an in-order subsequence of the input")
		}
		var valid []canonical.Trade
		for _, tr := range trades {
			if _, ok := validPrice(tr); ok {
				valid = append(valid, tr)
			}
		}
		if sigma <= 0 || len(trades) < 3 {
			if !sameTrades(got, trades) {
				t.Fatal("sigma<=0 / <3 trades must return the input unchanged")
			}
			return
		}
		for _, tr := range got {
			if _, ok := validPrice(tr); !ok {
				t.Fatal("non-positive trade survived")
			}
		}
		if len(valid) < 3 {
			if !sameTrades(got, valid) {
				t.Fatal("<3 usable prices must return exactly the usable trades")
			}
			return
		}
		if len(valid)%2 == 1 && len(got) == 0 {
			t.Fatal("odd window dropped its own median")
		}
		if want := refFilterOutliers(valid, sigma); !sameKeys(got, want) {
			t.Fatalf("FilterOutliers kept %d, documented band keeps %d", len(got), len(want))
		}
		// Price scale invariance: multiplying every quote by k keeps the set.
		mult := big.NewInt(int64(k%50) + 2)
		scaled := cloneTrades(trades)
		for i := range scaled {
			q := scaled[i].QuoteAmount.BigInt()
			scaled[i].QuoteAmount = canonical.NewAmount(q.Mul(q, mult))
		}
		if gs := FilterOutliers(scaled, sigma); !sameKeys(gs, got) {
			t.Fatalf("filter not scale-invariant: %d vs %d survivors", len(gs), len(got))
		}
		// Sigma monotonicity: a wider band never drops a survivor.
		if wider := FilterOutliers(trades, sigma*2); !isSubsequence(got, wider) {
			t.Fatal("doubling sigma dropped a survivor")
		}
	})
}

// refMedian is a sort-free order-statistic median (independent of medianRat).
func refMedian(vals []*big.Rat) *big.Rat {
	n := len(vals)
	kth := func(k int) *big.Rat {
		for _, v := range vals {
			less, eq := 0, 0
			for _, w := range vals {
				switch w.Cmp(v) {
				case -1:
					less++
				case 0:
					eq++
				}
			}
			if less <= k && k < less+eq {
				return v
			}
		}
		return nil
	}
	if n%2 == 1 {
		return new(big.Rat).Set(kth(n / 2))
	}
	s := new(big.Rat).Add(kth(n/2-1), kth(n/2))
	return s.Quo(s, big.NewRat(2, 1))
}

// refFilterOutliers keeps the usable trades inside the band documented on
// FilterOutliers: [c²/(c+T), c+T] with T = sigma·1.4826·MAD, or
// sigma·c/200 when the MAD is zero.
func refFilterOutliers(valid []canonical.Trade, sigma float64) []canonical.Trade {
	prices := make([]*big.Rat, len(valid))
	for i, tr := range valid {
		prices[i], _ = validPrice(tr)
	}
	c := refMedian(prices)
	devs := make([]*big.Rat, len(prices))
	for i, p := range prices {
		devs[i] = new(big.Rat).Abs(new(big.Rat).Sub(p, c))
	}
	scale := new(big.Rat).Mul(big.NewRat(14826, 10000), refMedian(devs))
	if scale.Sign() == 0 {
		scale = new(big.Rat).Quo(c, big.NewRat(200, 1))
	}
	hi := new(big.Rat).Add(c, new(big.Rat).Mul(new(big.Rat).SetFloat64(sigma), scale))
	lo := new(big.Rat).Quo(new(big.Rat).Mul(c, c), hi)
	var out []canonical.Trade
	for i, p := range prices {
		if p.Cmp(lo) >= 0 && p.Cmp(hi) <= 0 {
			out = append(out, valid[i])
		}
	}
	return out
}

func sameKeys(a, b []canonical.Trade) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TxHash != b[i].TxHash {
			return false
		}
	}
	return true
}

// FuzzDecimalsAdjustment compares the factor against a decimal-string
// reference and checks AdjustPrice round-trips.
func FuzzDecimalsAdjustment(f *testing.F) {
	f.Add(int64(3), int64(7), int8(7), int8(7))
	f.Add(int64(3), int64(7), int8(7), int8(18))
	f.Add(int64(-9), int64(2), int8(18), int8(6))
	f.Add(int64(1), int64(1), int8(0), int8(-38))
	f.Fuzz(func(t *testing.T, num, den int64, bd, qd int8) {
		k := DecimalsAdjustment(int(bd), int(qd))
		want, ok := new(big.Rat).SetString(fmt.Sprintf("1e%d", int(bd)-int(qd)))
		if !ok {
			t.Fatalf("reference parse failed for %d", int(bd)-int(qd))
		}
		if k.Cmp(want) != 0 {
			t.Fatalf("DecimalsAdjustment(%d,%d) = %s, want %s", bd, qd, k.RatString(), want.RatString())
		}
		if inv := DecimalsAdjustment(int(qd), int(bd)); new(big.Rat).Mul(k, inv).Cmp(big.NewRat(1, 1)) != 0 {
			t.Fatalf("factors for (%d,%d) are not reciprocal", bd, qd)
		}
		if AdjustPrice(nil, int(bd), int(qd)) != nil {
			t.Fatal("AdjustPrice(nil) must be nil")
		}
		if den == 0 {
			return
		}
		raw := big.NewRat(num, den)
		rawStr := raw.RatString()
		adj := AdjustPrice(raw, int(bd), int(qd))
		if adj.Cmp(new(big.Rat).Mul(raw, want)) != 0 {
			t.Fatalf("AdjustPrice = %s, want %s", adj.RatString(), new(big.Rat).Mul(raw, want).RatString())
		}
		if raw.RatString() != rawStr {
			t.Fatal("AdjustPrice mutated its input")
		}
		if back := AdjustPrice(adj, int(qd), int(bd)); back.Cmp(raw) != 0 {
			t.Fatalf("round trip %s -> %s -> %s", rawStr, adj.RatString(), back.RatString())
		}
	})
}

// FuzzNormalizeAmountScale checks per-trade price preservation, exact
// 10^k lifts, and that the normalised VWAP equals the real-volume VWAP.
func FuzzNormalizeAmountScale(f *testing.F) {
	f.Add(seedTradeBytes(100, 200, 100, 300, 100, 250), uint8(7), uint8(8), uint8(6))
	f.Add(seedTradeBytes(100, 200, 100, 300), uint8(7), uint8(7), uint8(7))
	f.Add(seedTradeBytes(1, 1, 5, 7, 9, 11, 3, 3), uint8(0), uint8(18), uint8(38))
	f.Fuzz(func(t *testing.T, data []byte, da, db, dc uint8) {
		trades := fuzzTrades(data)
		orig := cloneTrades(trades)
		decs := map[string]int{"a": int(da % 39), "b": int(db % 39), "c": int(dc % 39)}
		got := NormalizeAmountScale(trades, func(s string) int { return decs[s] })
		if !sameTrades(trades, orig) {
			t.Fatal("input trades mutated")
		}
		if len(got) != len(trades) {
			t.Fatalf("len %d, want %d", len(got), len(trades))
		}
		maxDec := 0
		for _, tr := range trades {
			if decs[tr.Source] > maxDec {
				maxDec = decs[tr.Source]
			}
		}
		realQ, realB := new(big.Rat), new(big.Rat)
		for i, tr := range trades {
			lift := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(maxDec-decs[tr.Source])), nil)
			wb := new(big.Int).Mul(tr.BaseAmount.BigInt(), lift)
			wq := new(big.Int).Mul(tr.QuoteAmount.BigInt(), lift)
			if got[i].BaseAmount.BigInt().Cmp(wb) != 0 || got[i].QuoteAmount.BigInt().Cmp(wq) != 0 {
				t.Fatalf("trade %d lifted to %s/%s, want %s/%s", i, got[i].BaseAmount, got[i].QuoteAmount, wb, wq)
			}
			if got[i].TxHash != tr.TxHash || !got[i].Timestamp.Equal(tr.Timestamp) {
				t.Fatalf("trade %d identity changed", i)
			}
			if p, ok := validPrice(tr); ok {
				gp, _ := validPrice(got[i])
				if gp.Cmp(p) != 0 {
					t.Fatalf("trade %d price moved %s -> %s", i, p.RatString(), gp.RatString())
				}
				unit := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decs[tr.Source])), nil))
				realB.Add(realB, new(big.Rat).Quo(new(big.Rat).SetInt(tr.BaseAmount.BigInt()), unit))
				realQ.Add(realQ, new(big.Rat).Quo(new(big.Rat).SetInt(tr.QuoteAmount.BigInt()), unit))
			}
		}
		if realB.Sign() > 0 {
			v, err := VWAP(got)
			if err != nil {
				t.Fatalf("VWAP: %v", err)
			}
			if want := new(big.Rat).Quo(realQ, realB); v.Cmp(want) != 0 {
				t.Fatalf("normalised VWAP %s, want real-volume VWAP %s", v.RatString(), want.RatString())
			}
		}
	})
}

// FuzzTriangulateChain pins the chain to a product that is order
// independent and rejects any non-positive leg.
func FuzzTriangulateChain(f *testing.F) {
	addRatSeeds(f)
	f.Add([]byte{0xc0, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzBytes{b: data}
		var legs []*big.Rat
		for len(legs) < 8 {
			num, ok1 := r.bigInt()
			den, ok2 := r.bigInt()
			if !ok1 || !ok2 {
				break
			}
			if den.Sign() == 0 {
				den.SetInt64(1)
			}
			legs = append(legs, new(big.Rat).SetFrac(num, den))
		}
		want := big.NewRat(1, 1)
		allPos := len(legs) > 0
		for _, l := range legs {
			if l.Sign() <= 0 {
				allPos = false
			}
			want.Mul(want, l)
		}
		snap := make([]string, len(legs))
		for i, l := range legs {
			snap[i] = l.RatString()
		}
		got, err := TriangulateChain(legs...)
		if !allPos {
			if !errors.Is(err, ErrTriangulateZero) || got != nil {
				t.Fatalf("TriangulateChain(%v) = (%v, %v), want ErrTriangulateZero", snap, got, err)
			}
		} else {
			if err != nil || got.Cmp(want) != 0 {
				t.Fatalf("TriangulateChain(%v) = (%v, %v), want %s", snap, got, err, want.RatString())
			}
			rev := make([]*big.Rat, len(legs))
			for i := range legs {
				rev[len(legs)-1-i] = legs[i]
			}
			if g2, _ := TriangulateChain(rev...); g2.Cmp(got) != 0 {
				t.Fatal("chain product depends on leg order")
			}
			got.Add(got, big.NewRat(1, 1)) // result must be independent of the inputs
		}
		if len(legs) == 2 {
			g, err2 := Triangulate(legs[0], legs[1])
			if allPos != (err2 == nil) || (err2 == nil && g.Cmp(want) != 0) {
				t.Fatalf("Triangulate(%v) = (%v, %v), want %s", snap, g, err2, want.RatString())
			}
		}
		for i, l := range legs {
			if l.RatString() != snap[i] {
				t.Fatal("legs mutated")
			}
		}
	})
}

func fuzzOracleRows(data []byte) []canonical.OracleUpdate {
	r := &fuzzBytes{b: data}
	var out []canonical.OracleUpdate
	for len(out) < 12 {
		d, ok := r.next()
		if !ok {
			break
		}
		p, ok := r.bigInt()
		if !ok {
			break
		}
		out = append(out, canonical.OracleUpdate{
			Source:    fmt.Sprintf("src%d", len(out)),
			Price:     canonical.NewAmount(p),
			Decimals:  d % 39,
			Timestamp: fuzzT0.Add(time.Duration(d) * time.Second),
		})
	}
	return out
}

func addOracleSeeds(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{8, 3, 0x05, 0xf5, 0xe1, 0x00})
	f.Add([]byte{8, 3, 0x05, 0xf5, 0xe1, 0x00, 8, 3, 0x05, 0xf6, 0x00, 0x00, 14, 7, 0, 0x5a, 0xf3, 0x10, 0x7a, 0x40, 0, 0})
	f.Add([]byte{8, 3, 0x05, 0xf5, 0xe1, 0x00, 8, 3, 0x05, 0xf5, 0xe1, 0x00, 8, 3, 0x0b, 0xeb, 0xc2, 0x00})
	f.Add([]byte{20, 0, 1})
	f.Add([]byte{20, 0, 1, 8, 0, 1})
	// Two far-apart sources; a third that is negative; zero prices.
	f.Add([]byte{8, 0, 1, 9, 1, 0x03, 0xe8})
	f.Add([]byte{8, 0, 1, 8, 1, 0x03, 0xe8, 8, 0x80, 5})
	f.Add([]byte{8, 0x40, 1, 8, 0, 1})
	f.Add([]byte{16, 1, 0x03, 0xe8, 8, 0, 1, 8, 0, 1})
}

// FuzzAverageAggregatorPrices checks the aggregator-tier mean against
// the exact rational mean of the real prices, and that a served price
// is always strictly positive.
func FuzzAverageAggregatorPrices(f *testing.F) {
	addOracleSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		rows := fuzzOracleRows(data)
		sum := new(big.Rat)
		n := 0
		var latest time.Time
		for _, u := range rows {
			p := u.Price.BigInt()
			if p.Sign() <= 0 {
				continue
			}
			unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(u.Decimals)), nil)
			sum.Add(sum, new(big.Rat).SetFrac(p, unit))
			n++
			if u.Timestamp.After(latest) {
				latest = u.Timestamp
			}
		}
		s, asOf, ok := averageAggregatorPrices(rows)
		if !ok {
			if s != "" {
				t.Fatalf("ok=false with price %q", s)
			}
			return
		}
		if n == 0 {
			t.Fatalf("ok=true with no positive price: %q", s)
		}
		if !asOf.Equal(latest) {
			t.Fatalf("asOf %v, want %v", asOf, latest)
		}
		if i := strings.IndexByte(s, '.'); i < 0 || len(s)-i-1 != aggregatorCommonDecimals {
			t.Fatalf("price %q not rendered at %d dp", s, aggregatorCommonDecimals)
		}
		got, okp := new(big.Rat).SetString(s)
		if !okp {
			t.Fatalf("unparseable price %q", s)
		}
		if got.Sign() <= 0 {
			t.Fatalf("served aggregator price %q is not positive (exact mean of %d sources = %s)",
				s, n, new(big.Rat).Quo(sum, big.NewRat(int64(n), 1)).FloatString(20))
		}
		mean := new(big.Rat).Quo(sum, big.NewRat(int64(n), 1))
		if got.Cmp(mean) > 0 {
			t.Fatalf("price %s exceeds the exact mean %s (truncation must round down)", s, mean.FloatString(20))
		}
		// The rendered price is the exact mean floored once at 14 dp.
		ulp := new(big.Rat).SetFrac(big.NewInt(1), pow10(aggregatorCommonDecimals))
		if new(big.Rat).Sub(mean, got).Cmp(ulp) >= 0 {
			t.Fatalf("price %s is 1 ulp or more below the exact mean %s", s, mean.FloatString(20))
		}
	})
}

// FuzzFormatScaledDecimal round-trips the fixed-point renderer.
func FuzzFormatScaledDecimal(f *testing.F) {
	f.Add(int64(100_010_000_000_000), uint8(14))
	f.Add(int64(5), uint8(14))
	f.Add(int64(0), uint8(3))
	f.Add(int64(123456789), uint8(0))
	f.Fuzz(func(t *testing.T, v int64, d uint8) {
		if v < 0 {
			v = -(v + 1) // non-negative: every caller passes a mean of positive prices
		}
		dec := int(d % 39)
		if dec == 0 {
			dec = 1
		}
		p := pow10(dec)
		s := formatScaledDecimal(big.NewInt(v), p, dec)
		if i := strings.IndexByte(s, '.'); i < 0 || len(s)-i-1 != dec {
			t.Fatalf("%q not rendered at %d dp", s, dec)
		}
		got, ok := new(big.Rat).SetString(s)
		if !ok || got.Cmp(new(big.Rat).SetFrac(big.NewInt(v), p)) != 0 {
			t.Fatalf("formatScaledDecimal(%d, 10^%d) = %q", v, dec, s)
		}
	})
}

// FuzzRejectAggregatorOutliers checks the divergence filter is an
// in-order subset that never empties and always keeps the median.
func FuzzRejectAggregatorOutliers(f *testing.F) {
	addOracleSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		rows := fuzzOracleRows(data)
		got := rejectAggregatorOutliers(rows)
		if len(rows) > 0 && len(got) == 0 {
			t.Fatal("filter emptied a non-empty source set")
		}
		j := 0
		for _, g := range got {
			for j < len(rows) && rows[j].Source != g.Source {
				j++
			}
			if j == len(rows) {
				t.Fatal("output is not an in-order subset of the input")
			}
			j++
		}
		var vals []*big.Rat
		for i := range rows {
			v, ok := scaledAggregatorRat(&rows[i])
			if ok != (rows[i].Price.Sign() > 0) {
				t.Fatalf("scaledAggregatorRat(%s) ok=%v, want ok iff the price is positive", rows[i].Price, ok)
			}
			if ok {
				vals = append(vals, v)
				// Reference: the real price at 14 dp, truncated.
				unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(rows[i].Decimals)), nil)
				want := new(big.Int).Mul(rows[i].Price.BigInt(), pow10(aggregatorCommonDecimals))
				want.Quo(want, unit)
				if v.Cmp(new(big.Rat).SetInt(want)) != 0 {
					t.Fatalf("scaledAggregatorRat(%s @%d) = %s, want %s", rows[i].Price, rows[i].Decimals, v.RatString(), want)
				}
			}
		}
		if len(vals) < aggregatorMinForOutlierReject {
			if len(got) != len(rows) {
				t.Fatal("fewer than 3 usable sources must pass through unchanged")
			}
			return
		}
		if len(vals)%2 == 1 && medianRat(vals).Sign() > 0 {
			m := medianRat(vals)
			kept := false
			for i := range got {
				if v, ok := scaledAggregatorRat(&got[i]); ok && v.Cmp(m) == 0 {
					kept = true
				}
			}
			if !kept {
				t.Fatalf("median source %s dropped", m.RatString())
			}
		}
	})
}
