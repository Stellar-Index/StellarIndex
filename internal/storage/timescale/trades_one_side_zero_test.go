package timescale

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// osztAmt is a terse canonical.Amount for the tables below.
func osztAmt(n int64) canonical.Amount { return canonical.NewAmount(big.NewInt(n)) }

// osztPair builds the native→USDC pair every case in this file trades on.
func osztPair(t *testing.T) canonical.Pair {
	t.Helper()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

// osztHash returns a valid 64-char lowercase-hex tx hash seeded by n.
func osztHash(n uint32) string { return fmt.Sprintf("%064x", n) }

// TestIsOneSideZeroFill pins the predicate that separates the EXPECTED,
// benign SDEX rounding artifact (exactly one leg == 0) from every other
// Validate failure. It is the W1-defi-1 classifier: a false positive would
// silence a genuine decoder bug; a false negative would re-fire the spurious
// insert-error alert on an ordinary one-side-zero fill.
func TestIsOneSideZeroFill(t *testing.T) {
	t.Parallel()
	pair := osztPair(t)
	mk := func(base, quote int64) canonical.Trade {
		return canonical.Trade{Pair: pair, BaseAmount: osztAmt(base), QuoteAmount: osztAmt(quote)}
	}
	cases := []struct {
		name string
		t    canonical.Trade
		want bool
	}{
		{"quote leg zero", mk(100, 0), true},
		{"base leg zero", mk(0, 100), true},
		{"both legs positive", mk(100, 25), false},
		{"both legs zero", mk(0, 0), false},
		{"negative base leg", mk(-100, 25), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isOneSideZeroFill(tc.t); got != tc.want {
				t.Errorf("isOneSideZeroFill(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestFilterStorableTrades proves the batch pre-filter that stops a
// one-side-zero fill from sinking the whole all-or-nothing INSERT
// (W1-defi-1):
//
//   - valid rows pass through, order preserved;
//   - a one-side-zero fill is dropped as a BENIGN no-op — it must NOT bump
//     SourceInsertErrorsTotal (the counter behind
//     stellarindex_source_insert_errors_total, the spurious alert);
//   - any OTHER Validate failure (here: a malformed tx_hash) is dropped AND
//     counted loudly, exactly as the single-row InsertTrade path surfaces it.
func TestFilterStorableTrades(t *testing.T) {
	pair := osztPair(t)
	ts := time.Now().UTC().Add(-time.Hour)
	valid := func(txHash string) canonical.Trade {
		return canonical.Trade{
			Source: "sdex", Ledger: 60_000_000, TxHash: txHash, OpIndex: 0,
			Timestamp: ts, Pair: pair,
			BaseAmount: osztAmt(1_000), QuoteAmount: osztAmt(25),
		}
	}
	valid1 := valid(osztHash(1))
	valid2 := valid(osztHash(2))

	oneSideZero := valid(osztHash(3))
	oneSideZero.QuoteAmount = osztAmt(0) // quote leg rounded to 0 — a real fill the served tier can't hold

	badHash := valid("not-a-hash") // Validate fails for a NON-amount reason → must stay loud

	s := &Store{}
	const src, kind = "sdex", "trade"
	before := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(src, kind))

	got := s.filterStorableTrades([]canonical.Trade{valid1, oneSideZero, valid2, badHash})

	if len(got) != 2 {
		t.Fatalf("storable count = %d, want 2 (the two valid trades; the zero-leg + bad-hash rows must be filtered)", len(got))
	}
	if got[0].TxHash != valid1.TxHash || got[1].TxHash != valid2.TxHash {
		t.Errorf("storable order/content = [%s, %s], want [%s, %s]",
			got[0].TxHash, got[1].TxHash, valid1.TxHash, valid2.TxHash)
	}

	after := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(src, kind))
	if delta := after - before; delta != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,trade} delta = %v, want 1 "+
			"(ONLY the malformed-tx_hash row is an error; the one-side-zero fill must be a benign skip)", delta)
	}
}

// TestFilterStorableTrades_AllValidFastPath proves the common case returns the
// input untouched with no metric noise.
func TestFilterStorableTrades_AllValidFastPath(t *testing.T) {
	pair := osztPair(t)
	ts := time.Now().UTC().Add(-time.Hour)
	mk := func(txHash string) canonical.Trade {
		return canonical.Trade{
			Source: "sdex", Ledger: 60_000_000, TxHash: txHash, OpIndex: 0,
			Timestamp: ts, Pair: pair,
			BaseAmount: osztAmt(1_000), QuoteAmount: osztAmt(25),
		}
	}
	in := []canonical.Trade{mk(osztHash(10)), mk(osztHash(11))}
	s := &Store{}

	before := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	got := s.filterStorableTrades(in)
	after := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))

	if len(got) != len(in) {
		t.Fatalf("all-valid storable count = %d, want %d", len(got), len(in))
	}
	if after != before {
		t.Errorf("all-valid batch bumped SourceInsertErrorsTotal by %v, want 0", after-before)
	}
}
