package lendinginterest

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

// defaultFold folds the mainnet defaulted_debt versions in ds, each seeded with its transaction's pre-image, beside
// extra versions; with events, each version's own defaulted_debt event is read.
func defaultFold(t *testing.T, ds []blendtest.Default, extra []Version, events bool, more ...Event) []HourRow {
	t.Helper()
	seed := Seeds{Reserves: map[ReserveKey][]blend.ReserveData{}, Configs: map[string][]uint32{}}
	opts := Options{Protocols: map[string]Protocol{}}
	var vs []Version
	for i, d := range ds {
		seed.Reserves[ReserveKey{d.Pool, d.Asset}] = []blend.ReserveData{reserve(t, d.Prev, blend.PoolV2)}
		seed.Configs[d.Pool] = []uint32{d.BstopRate}
		opts.Protocols[d.Pool] = BlendV2
		v := Version{
			Pool: d.Pool, Kind: KindReserve, Asset: d.Asset, Ledger: d.Ledger, IntraLedgerSeq: uint32(i + 1), //nolint:gosec // a few versions
			TxHash: d.Tx, CloseTime: unix(d.Next.LastTime), Reserve: reserve(t, d.Next, blend.PoolV2),
		}
		vs = append(vs, v)
		if events {
			opts.Events = append(opts.Events, defaultEvent(v, d.EventIndex, dec(t, d.DTokens)))
		}
	}
	opts.Events = append(opts.Events, more...)
	return fold(t, seed, append(vs, extra...), opts)
}

func defaultEvent(v Version, index uint32, dTokens *big.Int) Event {
	return Event{
		Pool: v.Pool, Ledger: v.Ledger, TxHash: v.TxHash, OpIndex: v.OpIndex, EventIndex: index, CloseTime: v.CloseTime,
		Cause: CauseDefaultedDebt, Asset: v.Asset, DTokens: dTokens,
	}
}

func rowOf(t *testing.T, rows []HourRow, pool, asset string, ledger uint32) HourRow {
	t.Helper()
	for _, r := range rows {
		if r.Pool == pool && r.Asset == asset && r.FirstLedger <= ledger && ledger <= r.LastLedger {
			return r
		}
	}
	t.Fatalf("no row of %s %s at %d", pool, asset, ledger)
	return HourRow{}
}

// All 8 mainnet versions the full-history proof left unexplained are explained by their transaction's
// defaulted_debt, and each one's supply residual (the value production stored) is its default loss. Without the
// events, or with the transaction poisoned, every one stays unexplained and no loss is recorded.
func TestFold_DefaultedDebtMainnet(t *testing.T) {
	// One fold per transaction: a reserve's two transactions have other versions between them.
	byTx := map[string][]blendtest.Default{}
	for _, d := range blendtest.DefaultedDebt {
		byTx[d.Tx] = append(byTx[d.Tx], d)
	}
	if len(byTx) != 3 {
		t.Fatalf("%d transactions, want 3", len(byTx))
	}
	for _, d := range blendtest.DefaultedDebt {
		r := rowOf(t, defaultFold(t, byTx[d.Tx], nil, true), d.Pool, d.Asset, d.Ledger)
		if r.BRateMismatches != 1 || r.SupplyResidual.String() != d.SupplyResidual || r.DefaultLoss.String() != d.SupplyResidual ||
			r.DefaultVersions != 1 || r.ExplainedVersions != 1 || r.UnexplainedVersions != 0 || r.UnreconciledVersions != 1 {
			t.Errorf("%d %s: mismatches %d, supply residual %s (production %s), default loss %s over %d, explained %d, unexplained %d",
				d.Ledger, d.Asset, r.BRateMismatches, r.SupplyResidual, d.SupplyResidual, r.DefaultLoss, r.DefaultVersions,
				r.ExplainedVersions, r.UnexplainedVersions)
		}
	}
	d := blendtest.DefaultedDebt[0]
	none := defaultFold(t, byTx[d.Tx], nil, false)
	poison := Event{Pool: d.Pool, Ledger: d.Ledger, TxHash: d.Tx, EventIndex: 9, CloseTime: unix(d.Next.LastTime), Cause: CauseUndecodable}
	poisoned := defaultFold(t, blendtest.DefaultedDebt[:2], nil, true, poison)
	for name, rs := range map[string][]HourRow{"no events": none, "poisoned": poisoned} {
		for _, r := range rs {
			if r.UnexplainedVersions != 1 || r.DefaultVersions != 0 || r.DefaultLoss.Sign() != 0 {
				t.Errorf("%s: %s %s explained %d, default %d %s", name, r.Pool, r.Asset, r.ExplainedVersions, r.DefaultVersions, r.DefaultLoss)
			}
		}
	}
}

// A default beside an interest fill in one transaction: the b_rate side is the default's, the backstop side the
// fill's lot through classify; without the lot the version stays unexplained and its loss is not recorded.
func TestFold_DefaultedDebtWithInterestFill(t *testing.T) {
	d := blendtest.DefaultedDebt[3]
	d.Next.BackstopCredit = new(big.Int).Sub(dec(t, d.Next.BackstopCredit), big.NewInt(2220437757)).String()
	fill := Event{
		Pool: d.Pool, Ledger: d.Ledger, TxHash: d.Tx, EventIndex: 1, CloseTime: unix(d.Next.LastTime), Cause: CauseInterestFill,
		Asset: d.Asset, Amount: big.NewInt(-2220437757),
	}
	r := defaultFold(t, []blendtest.Default{d}, nil, true, fill)[0]
	if r.ExplainedVersions != 1 || r.ResidualExplained.String() != "-2220437757" || r.DefaultLoss.String() != d.SupplyResidual {
		t.Errorf("with the fill: explained %d %s, default loss %s", r.ExplainedVersions, r.ResidualExplained, r.DefaultLoss)
	}
	r = defaultFold(t, []blendtest.Default{d}, nil, true)[0]
	if r.UnexplainedVersions != 1 || r.ResidualUnexplained.String() != "-2220437757" || r.DefaultVersions != 0 || r.DefaultLoss.Sign() != 0 {
		t.Errorf("without the fill: unexplained %d %s, default %d %s", r.UnexplainedVersions, r.ResidualUnexplained, r.DefaultVersions, r.DefaultLoss)
	}
}

// The cut divides by the pre-transaction b_supply: a supply after it (mainnet 61341315's USDC reserve) leaves the
// version explained, but a b_rate cut over a b_supply the transaction had already moved does not match and stays
// unexplained.
func TestFold_DefaultedDebtBSupplyMovedFirst(t *testing.T) {
	d := blendtest.DefaultedDebt[1]
	if d.Prev.BSupply == d.Next.BSupply {
		t.Fatal("the golden version's b_supply does not move")
	}
	prev, next := reserve(t, d.Prev, blend.PoolV2), reserve(t, d.Next, blend.PoolV2)
	a, err := blend.AccrueBetween(prev, next, d.BstopRate)
	if err != nil {
		t.Fatal(err)
	}
	b, err := blend.DefaultedBRate(blend.ReserveData{DRate: next.DRate, BRate: a.ExpectedBRate, BSupply: next.BSupply, Version: blend.PoolV2},
		dec(t, d.DTokens))
	if err != nil {
		t.Fatal(err)
	}
	moved := d
	moved.Next.BRate = b.String()
	if moved.Next.BRate == d.Next.BRate {
		t.Fatal("the cut over the moved b_supply is the stored one")
	}
	r := defaultFold(t, []blendtest.Default{moved}, nil, true)[0]
	if r.UnexplainedVersions != 1 || r.DefaultVersions != 0 || r.DefaultLoss.Sign() != 0 || r.BRateMismatches != 1 {
		t.Errorf("explained %d, default %d %s", r.ExplainedVersions, r.DefaultVersions, r.DefaultLoss)
	}
}

// One hour row holding a default-explained version and a later b_rate cut no event explains: the row's supply
// residual less its default loss is the unexplained cut's residual.
func TestFold_DefaultedDebtMixedHour(t *testing.T) {
	d := blendtest.DefaultedDebt[3]
	later := Version{
		Pool: d.Pool, Kind: KindReserve, Asset: d.Asset, Ledger: d.Ledger + 2, IntraLedgerSeq: 1, TxHash: "later",
		CloseTime: unix(d.Next.LastTime).Add(10 * time.Second), Reserve: reserve(t, d.Next, blend.PoolV2),
	}
	later.Reserve.BRate = new(big.Int).Sub(later.Reserve.BRate, big.NewInt(1_000_000))
	r := defaultFold(t, []blendtest.Default{d}, []Version{later}, true)[0]
	unexplained := new(big.Int).Sub(r.SupplyResidual, r.DefaultLoss)
	if r.BRateMismatches != 2 || r.DefaultVersions != 1 || r.UnexplainedVersions != 1 || r.DefaultLoss.String() != d.SupplyResidual ||
		unexplained.Sign() >= 0 {
		t.Errorf("mismatches %d, default %d %s, unexplained %d, supply residual %s", r.BRateMismatches, r.DefaultVersions, r.DefaultLoss,
			r.UnexplainedVersions, r.SupplyResidual)
	}
}
