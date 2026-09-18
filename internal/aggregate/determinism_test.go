package aggregate

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Finding K036: the aggregator assembles a window by appending one
// batch per expanded source pair, and that expansion came out of Go map
// iteration ([FiatBackers]); the local outlier index then sorted only on
// Timestamp, so same-timestamp prints — the common case, since every
// trade in a ledger shares its close time — kept the merge order. The
// index references a print's neighbours BY POSITION, so the reference
// centre, the score and the trim all moved tick to tick and a window's
// VWAP was not reproducible from its own inputs.

func TestFiatBackers_DeterministicOrder(t *testing.T) {
	// Map iteration is randomised per range statement, so repeating the
	// call is what catches an unsorted return.
	want := FiatBackers("USD")
	if len(want) < 2 {
		t.Fatalf("FiatBackers(USD) = %v — need at least two backers to test ordering", want)
	}
	for i := 1; i < len(want); i++ {
		if want[i-1] >= want[i] {
			t.Fatalf("FiatBackers(USD) = %v — not sorted; the orchestrator's fetch plan "+
				"and therefore its merge order would differ between calls", want)
		}
	}
	for i := 0; i < 64; i++ {
		got := FiatBackers("USD")
		if len(got) != len(want) {
			t.Fatalf("call %d returned %d backers, want %d", i, len(got), len(want))
		}
		for k := range got {
			if got[k] != want[k] {
				t.Fatalf("call %d = %v, want %v — FiatBackers is not deterministic", i, got, want)
			}
		}
	}
}

func TestExpandTargetPair_DeterministicOrder(t *testing.T) {
	base, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	target, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	want, err := ExpandTargetPair(target)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for i := 0; i < 64; i++ {
		got, err := ExpandTargetPair(target)
		if err != nil {
			t.Fatalf("expand %d: %v", i, err)
		}
		for k := range got {
			if !got[k].Equal(want[k]) {
				t.Fatalf("expansion %d = %v, want %v — the fetch plan (and the merge order "+
					"of the window it assembles) is not reproducible", i, got, want)
			}
		}
	}
}

// TestNewLocalIndex_OrderIndependentOfInputOrder pins the tie-break
// itself: the index's time order must be a function of the TRADE SET,
// not of the order the caller merged it in.
func TestNewLocalIndex_OrderIndependentOfInputOrder(t *testing.T) {
	ts := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	// Four prints sharing one ledger-close timestamp, from two sources,
	// plus one a minute later. Distinct PKs, deliberately NOT in PK order.
	set := []canonical.Trade{
		mkOrderedTrade("sdex", 900, "cc", 1, ts, 101),
		mkOrderedTrade("soroswap", 900, "aa", 0, ts, 99),
		mkOrderedTrade("sdex", 900, "bb", 0, ts, 100),
		mkOrderedTrade("sdex", 899, "zz", 3, ts, 102),
		mkOrderedTrade("sdex", 901, "dd", 0, ts.Add(time.Minute), 100),
	}
	// PK order at the shared timestamp: ledger 899 first, then ledger 900
	// by source ("sdex" < "soroswap") and tx_hash ("bb" < "cc").
	want := []string{"zz", "bb", "cc", "aa", "dd"}

	permutations := [][]int{
		{0, 1, 2, 3, 4},
		{4, 3, 2, 1, 0},
		{2, 0, 4, 1, 3},
		{1, 3, 0, 4, 2},
	}
	for _, perm := range permutations {
		trades := make([]canonical.Trade, len(perm))
		for i, p := range perm {
			trades[i] = set[p]
		}
		validIdx := make([]int, len(trades))
		prices := make([]*big.Rat, len(trades))
		for i := range trades {
			validIdx[i] = i
			p, ok := priceRat(&trades[i])
			if !ok {
				t.Fatalf("price for %+v", trades[i])
			}
			prices[i] = p
		}
		ix := newLocalIndex(trades, validIdx, prices, LocalOutlierOptions{Sigma: 4}.withDefaults())

		got := make([]string, len(ix.order))
		for k, pos := range ix.order {
			got[k] = trades[validIdx[pos]].TxHash
		}
		for k := range want {
			if got[k] != want[k] {
				t.Fatalf("input permutation %v produced order %v, want the trades-PK order %v — "+
					"the neighbourhood reference is taken by POSITION, so the merge order "+
					"would decide the trim", perm, got, want)
			}
		}
	}
}

// mkOrderedTrade builds a trade with an explicit primary key and price
// (base 1 unit, so quote == price).
func mkOrderedTrade(source string, ledger uint32, txHash string, opIndex uint32, ts time.Time, quote int64) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      ledger,
		TxHash:      txHash,
		OpIndex:     opIndex,
		Timestamp:   ts,
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}
