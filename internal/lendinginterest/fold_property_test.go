package lendinginterest

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

var genPools = []struct {
	pool   string
	proto  Protocol
	assets []string
}{
	{"P1", BlendV2, []string{"A", "B"}},
	{"P2", BLNT, []string{"A", "C"}},
	{"P3", BlendV1, []string{"A"}},
}

// gen is a deterministic synthetic lake: several pools and assets, keys silent for hours, Config and
// BLNT fee writes (in the reserve's transaction and alone), unordered ledgers whose later writes agree
// or disagree, evictions followed by a restore, and re-derive orphans.
type gen struct {
	rng           *rand.Rand
	res           map[ReserveKey]blend.ReserveData
	fees          map[string]blnt.ProtocolFees
	evicted       map[ReserveKey]bool
	cfg           map[string]uint32
	out           []Version
	events        []Event
	ledger        uint32
	now           time.Time
	intra, change uint32
}

func generate(seed int64) ([]Version, Seeds, Options) {
	g := &gen{
		rng: rand.New(rand.NewSource(seed)), //nolint:gosec // deterministic test data
		res: map[ReserveKey]blend.ReserveData{}, fees: map[string]blnt.ProtocolFees{"P2": {}},
		evicted: map[ReserveKey]bool{}, ledger: 1000, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), cfg: map[string]uint32{},
	}
	opts := Options{Protocols: map[string]Protocol{}}
	seeds := Seeds{Configs: map[string][]uint32{}, Fees: map[string][]blnt.ProtocolFees{"P2": {{}}}}
	for _, p := range genPools {
		opts.Protocols[p.pool] = p.proto
		seeds.Configs[p.pool] = []uint32{2_000_000}
		g.cfg[p.pool] = 2_000_000
	}
	steps := []time.Duration{5 * time.Second, 3 * time.Minute, 25 * time.Minute, 5 * time.Hour}
	for range 30 + g.rng.Intn(30) {
		g.ledger += 1 + uint32(g.rng.Intn(3)) //nolint:gosec // < 3
		g.now = g.now.Add(steps[g.rng.Intn(len(steps))])
		g.ledgerVersions()
	}
	opts.Events = g.events
	return g.out, seeds, opts
}

func (g *gen) ledgerVersions() {
	start := len(g.out)
	g.intra = 0
	for range 1 + g.rng.Intn(3) {
		g.tx()
	}
	if g.rng.Intn(10) == 0 {
		g.evict()
	}
	if g.rng.Intn(5) == 0 {
		for i := start; i < len(g.out); i++ {
			g.out[i].IntraLedgerSeq = 0
		}
	}
	if g.rng.Intn(6) == 0 && len(g.out) > start {
		v := g.out[start+g.rng.Intn(len(g.out)-start)]
		v.ChangeIndex += 50
		switch g.rng.Intn(3) {
		case 0:
			v.IntraLedgerSeq += 100
		case 1: // ahead of the ledger's first transaction: the positions contradict
			v.IntraLedgerSeq = 1
		}
		g.out = append(g.out, v)
	}
}

func (g *gen) version(pool, tx string, kind Kind) Version {
	g.intra++
	g.change++
	return Version{Pool: pool, Kind: kind, Ledger: g.ledger, IntraLedgerSeq: g.intra, TxHash: tx, ChangeIndex: g.change, CloseTime: g.now}
}

func (g *gen) tx() {
	p := genPools[g.rng.Intn(len(genPools))]
	tx := fmt.Sprintf("%016x", g.rng.Uint64())
	g.change = 0
	feeOnly := p.proto == BLNT && g.rng.Intn(6) == 0
	if !feeOnly {
		for _, a := range p.assets {
			if g.rng.Intn(2) == 0 {
				g.reserve(p.pool, a, p.proto, tx)
			}
		}
		if g.rng.Intn(8) == 0 {
			v := g.version(p.pool, tx, KindConfig)
			v.BstopRate = uint32(1_000_000 * (1 + g.rng.Intn(2))) //nolint:gosec // small
			g.out = append(g.out, v)
			g.cfg[p.pool] = v.BstopRate // in force from the next transaction
		}
	}
	if p.proto == BLNT && (feeOnly || g.rng.Intn(3) > 0) {
		f := maps.Clone(g.fees[p.pool])
		for _, a := range p.assets {
			if g.rng.Intn(2) == 0 {
				e := feeEntry(f, a)
				f[a] = blnt.ProtocolFee{Credit: new(big.Int).Add(e.Credit, big.NewInt(g.rng.Int63n(3))), Carry: big.NewInt(g.rng.Int63n(10_000_000))}
			}
		}
		g.fees[p.pool] = f
		v := g.version(p.pool, tx, KindFees)
		v.Fees = f
		g.out = append(g.out, v)
	}
}

func (g *gen) reserve(pool, asset string, proto Protocol, tx string) {
	k := ReserveKey{pool, asset}
	pv, _ := proto.poolVersion()
	cur, ok := g.res[k]
	if g.evicted[k] { // the restored archived value, then the transaction's own write
		delete(g.evicted, k)
		v := g.version(pool, tx, KindReserve)
		v.Asset, v.Reserve = asset, cur
		g.out = append(g.out, v)
	}
	next := blend.ReserveData{IRMod: big.NewInt(1), Version: pv, LastTime: uint64(g.now.Unix())} //nolint:gosec // 2026
	switch {
	case !ok:
		next.DRate, next.BRate, next.BackstopCredit = big.NewInt(1_000_000_000), big.NewInt(1_000_000_000), new(big.Int)
	case cur.LastTime < next.LastTime:
		next.DRate = new(big.Int).Add(cur.DRate, big.NewInt(g.rng.Int63n(1_000_000)))
		next.BRate = new(big.Int).Add(cur.BRate, big.NewInt(g.rng.Int63n(1_000_000)))
		next.BackstopCredit = new(big.Int).Add(cur.BackstopCredit, big.NewInt(g.rng.Int63n(1000)))
	default: // a later transaction in the ledger: no accrual, sometimes a backstop credit draw
		next.DRate, next.BRate, next.BackstopCredit = cur.DRate, cur.BRate, cur.BackstopCredit
		if g.rng.Intn(3) == 0 {
			next.BackstopCredit = new(big.Int).Add(cur.BackstopCredit, big.NewInt(1))
		}
	}
	next.BSupply = big.NewInt(1_000_000_000_000 + g.rng.Int63n(1_000_000_000))
	next.DSupply = big.NewInt(500_000_000_000 + g.rng.Int63n(1_000_000_000))
	g.res[k] = next
	v := g.version(pool, tx, KindReserve)
	v.Asset, v.Reserve = asset, next
	g.out = append(g.out, v)
	if ok {
		g.causeEvent(v, proto, cur)
	}
}

// causeEvent sometimes adds an event to the reserve's transaction: on V2 a fill whose lot is the pair's
// residual as generated (explained when the pair is summed in that order), one unit off, or the same in another
// transaction of the ledger; a V1 fill (attributed when a draw); a BLNT gulp of a random amount.
func (g *gen) causeEvent(v Version, proto Protocol, prev blend.ReserveData) {
	e := Event{Pool: v.Pool, Ledger: v.Ledger, TxHash: v.TxHash, EventIndex: uint32(len(g.events)), CloseTime: v.CloseTime} //nolint:gosec // small
	switch proto {
	case BlendV1:
		if g.rng.Intn(3) > 0 {
			return
		}
		e.Cause = CauseInterestFillV1
	case BLNT:
		if g.rng.Intn(4) > 0 {
			return
		}
		e.Cause, e.Asset, e.Amount = CauseGulp, v.Asset, big.NewInt(g.rng.Int63n(5))
	default:
		amt := big.NewInt(1)
		if a, err := blend.AccrueBetween(prev, v.Reserve, g.cfg[v.Pool]); err == nil {
			amt = a.BackstopResidual
		}
		e.Cause, e.Asset, e.Amount = CauseInterestFill, v.Asset, amt
		switch g.rng.Intn(5) {
		case 0, 1:
		case 2:
			e.Amount = new(big.Int).Add(amt, big.NewInt(1))
		case 3:
			e.TxHash += "x"
		default:
			return
		}
	}
	g.events = append(g.events, e)
}

func (g *gen) evict() {
	keys := slices.SortedFunc(maps.Keys(g.res), func(a, b ReserveKey) int {
		if a.Pool != b.Pool {
			return cmpStr(a.Pool, b.Pool)
		}
		return cmpStr(a.Asset, b.Asset)
	})
	if len(keys) == 0 {
		return
	}
	k := keys[g.rng.Intn(len(keys))]
	if g.evicted[k] {
		return
	}
	g.evicted[k] = true
	v := g.version(k.Pool, "", KindReserve)
	v.Asset, v.Removed = k.Asset, true
	g.out = append(g.out, v)
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Folding generated multi-pool data in windows through the carry equals one fold, at every ledger cut
// and at random multi-cuts, including the MaxUnordered verdict at the worst row's count and one below.
func TestFold_SplitProperty(t *testing.T) {
	seeds := 150
	if testing.Short() {
		seeds = 40
	}
	var classes [3]uint32
	for s := range seeds {
		vs, seed, opts := generate(int64(s))
		opts.MaxUnordered = math.MaxInt
		rows, _, err := Fold(seed, vs, lastHour(vs), opts)
		if err != nil {
			t.Fatalf("seed %d: %v", s, err)
		}
		checkInvariant(t, rows)
		for _, r := range rows {
			classes[0] += r.ExplainedVersions
			classes[1] += r.AttributedVersions
			classes[2] += r.UnexplainedVersions
		}
		worst := 0
		for _, r := range rows {
			worst = max(worst, int(r.UnorderedVersions))
		}
		for _, limit := range []int{worst, worst - 1} {
			if limit >= 0 {
				opts.MaxUnordered = limit
				checkSplitProperty(t, s, seed, vs, opts)
			}
		}
	}
	// The generated data must reach every class, or split invariance of the event columns is untested.
	if classes[0] == 0 || classes[1] == 0 || classes[2] == 0 {
		t.Fatalf("explained, attributed, unexplained versions over all seeds: %v", classes)
	}
}

func checkSplitProperty(t *testing.T, s int, seed Seeds, vs []Version, opts Options) {
	t.Helper()
	final := lastHour(vs)
	one, oneErr := foldWindows(t, seed, vs, nil, false, final, opts)
	cuts := ledgerCuts(vs)
	rng := rand.New(rand.NewSource(int64(s))) //nolint:gosec // deterministic test data
	try := func(cs []int, whole bool) {
		got, err := foldWindows(t, seed, vs, cs, whole, final, opts)
		if (err != nil) != (oneErr != nil) || got != one {
			t.Fatalf("seed %d MaxUnordered %d cuts %v whole hours %v: split err %v, one fold err %v\n%s\nwant\n%s",
				s, opts.MaxUnordered, cs, whole, err, oneErr, got, one)
		}
	}
	for _, whole := range []bool{false, true} {
		if oneErr == nil {
			for _, c := range cuts {
				try([]int{c}, whole)
			}
		}
		for range 4 {
			var cs []int
			for _, c := range cuts {
				if rng.Intn(4) == 0 {
					cs = append(cs, c)
				}
			}
			try(cs, whole)
		}
	}
}
