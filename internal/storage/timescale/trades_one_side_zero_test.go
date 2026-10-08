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
// Validate failure. It is that classifier: a false positive would
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
			if got := IsOneSideZeroFill(tc.t); got != tc.want {
				t.Errorf("IsOneSideZeroFill(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestFilterStorableTrades proves the batch pre-filter that keeps one bad
// row from sinking the whole all-or-nothing INSERT:
//
//   - valid rows pass through, order preserved;
//   - a one-side-zero fill passes Validate and is ADMITTED (the served tier
//     stores it unpriceable) and counted on TradesZeroLegAdmittedTotal; it
//     must NOT bump SourceInsertErrorsTotal (the counter behind
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
	oneSideZero.QuoteAmount = osztAmt(0) // quote leg rounded to 0 — a real fill, stored unpriceable

	badHash := valid("not-a-hash") // Validate fails for a NON-amount reason → must stay loud

	s := &Store{}
	const src, kind = "sdex", "trade"
	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(src, kind))
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues(src))

	got := s.filterStorableTrades([]canonical.Trade{valid1, oneSideZero, valid2, badHash})

	if len(got) != 3 {
		t.Fatalf("storable count = %d, want 3 (two valid trades + the one-side-zero fill; only the bad-hash row is filtered)", len(got))
	}
	if got[0].TxHash != valid1.TxHash || got[1].TxHash != oneSideZero.TxHash || got[2].TxHash != valid2.TxHash {
		t.Errorf("storable order/content = [%s, %s, %s], want [%s, %s, %s]",
			got[0].TxHash, got[1].TxHash, got[2].TxHash, valid1.TxHash, oneSideZero.TxHash, valid2.TxHash)
	}

	errAfter := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(src, kind))
	if delta := errAfter - errBefore; delta != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,trade} delta = %v, want 1 "+
			"(ONLY the malformed-tx_hash row is an error; the one-side-zero fill is admitted)", delta)
	}
	zeroAfter := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues(src))
	if delta := zeroAfter - zeroBefore; delta != 1 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 1 (exactly the one-side-zero fill)", delta)
	}
}

// TestFilterStorableTrades_BothZeroStaysLoud pins that the relaxation is
// exactly one zero leg: a both-zero row still fails Validate, is dropped and
// counted as an insert error, and never reaches TradesZeroLegAdmittedTotal.
func TestFilterStorableTrades_BothZeroStaysLoud(t *testing.T) {
	pair := osztPair(t)
	bothZero := canonical.Trade{
		Source: "sdex", Ledger: 60_000_000, TxHash: osztHash(4), OpIndex: 0,
		Timestamp: time.Now().UTC().Add(-time.Hour), Pair: pair,
		BaseAmount: osztAmt(0), QuoteAmount: osztAmt(0),
	}
	s := &Store{}
	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))

	got := s.filterStorableTrades([]canonical.Trade{bothZero})

	if len(got) != 0 {
		t.Fatalf("storable count = %d, want 0 (both-zero is not a trade)", len(got))
	}
	if delta := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade")) - errBefore; delta != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,trade} delta = %v, want 1", delta)
	}
	if delta := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex")) - zeroBefore; delta != 0 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 0", delta)
	}
}

// TestFilterStorableTrades_AllValidFastPath proves the common case returns the
// input untouched with no error-metric noise, and that a one-side-zero fill in
// an all-valid batch is still counted on the fast path.
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
	zeroBase := mk(osztHash(12))
	zeroBase.BaseAmount = osztAmt(0)
	in := []canonical.Trade{mk(osztHash(10)), mk(osztHash(11)), zeroBase}
	s := &Store{}

	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))
	got := s.filterStorableTrades(in)
	errAfter := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	zeroAfter := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))

	if len(got) != len(in) {
		t.Fatalf("all-valid storable count = %d, want %d", len(got), len(in))
	}
	if errAfter != errBefore {
		t.Errorf("all-valid batch bumped SourceInsertErrorsTotal by %v, want 0", errAfter-errBefore)
	}
	if delta := zeroAfter - zeroBefore; delta != 1 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 1 (the zero-base fill on the fast path)", delta)
	}
}
