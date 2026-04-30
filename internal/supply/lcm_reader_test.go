package supply

import (
	"context"
	"errors"
	"math/big"
	"testing"
)

// fakeLookup satisfies AccountObservationLookup with an in-memory
// map for testing. nil row + non-nil err => "not found"
// scenario; nil err + populated row => happy path.
type fakeLookup struct {
	rows map[string]AccountObservationRow
	err  error
}

func (f *fakeLookup) LatestAccountObservationAtOrBefore(_ context.Context, accountID string, _ uint32) (AccountObservationRow, error) {
	if f.err != nil {
		return AccountObservationRow{}, f.err
	}
	row, ok := f.rows[accountID]
	if !ok {
		return AccountObservationRow{}, errors.New("no observation")
	}
	return row, nil
}

func TestLCMReserveBalanceReader_HappyPath(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50},
			"GA2": {Balance: big.NewInt(200), Ledger: 51},
		},
	})
	got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 100)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal: %v", err)
	}
	if got.Cmp(big.NewInt(300)) != 0 {
		t.Errorf("total=%s want 300", got)
	}
}

// TestLCMReserveBalanceReader_MissingAccountErrorsWithSentinel —
// when any account has no observation, the reader returns
// ErrNoObservation. The caller's chained-fallback handler keys
// on errors.Is(err, ErrNoObservation) to drop to the static path.
func TestLCMReserveBalanceReader_MissingAccountErrorsWithSentinel(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100)},
			// GA2 missing on purpose.
		},
	})
	_, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 100)
	if err == nil {
		t.Fatal("expected ErrNoObservation")
	}
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v not wrapping ErrNoObservation", err)
	}
}

// TestLCMReserveBalanceReader_RemovalCountsAsZero — a removed
// account contributes 0 to the total. Consistent with the on-
// chain post-state (the AccountEntry no longer exists, so its
// balance is necessarily 0).
func TestLCMReserveBalanceReader_RemovalCountsAsZero(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100)},
			"GA2": {IsRemoval: true},
		},
	})
	got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 100)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal: %v", err)
	}
	if got.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("total=%s want 100 (GA2 removed → 0)", got)
	}
}

// TestLCMReserveBalanceReader_StoreError — a generic storage
// error (not "not found") still returns wrapped in
// ErrNoObservation so the caller can fall back. We don't try to
// distinguish "transient DB error" from "no row" here — the
// fallback path is the same in both cases (use static config).
func TestLCMReserveBalanceReader_StoreError(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		err: errors.New("connection refused"),
	})
	_, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1"}, 100)
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v want wrapping ErrNoObservation", err)
	}
}
