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

// TestLCMReserveBalanceReader_MinReserveAccountLedger_HappyPath —
// returns MIN(row.Ledger) across the supplied accounts. F-1236
// (codex audit-2026-05-12): closes the third leg of the supply-
// snapshot freshness gate (the classic + SEP41 legs were shipped
// in waves 17 + 18).
func TestLCMReserveBalanceReader_MinReserveAccountLedger_HappyPath(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			"GA2": {Balance: big.NewInt(200), Ledger: 49_999_500},
			"GA3": {Balance: big.NewInt(300), Ledger: 50_000_100},
		},
	})
	got, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2", "GA3"}, 50_001_000)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got != 49_999_500 {
		t.Errorf("min=%d want 49_999_500 (GA2 oldest)", got)
	}
}

// TestLCMReserveBalanceReader_MinReserveAccountLedger_RemovalContributes
// — a removed-account observation IS the freshness signal for that
// account (the AccountEntry-delete is the most-recent observation we
// have). Pretending the removal didn't happen would over-state
// freshness for an account that no longer exists.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_RemovalContributes(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			"GA2": {IsRemoval: true, Ledger: 49_990_000}, // removal observation IS the signal
		},
	})
	got, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2"}, 50_001_000)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got != 49_990_000 {
		t.Errorf("min=%d want 49_990_000 (GA2 removal IS the freshness signal)", got)
	}
}

// TestLCMReserveBalanceReader_MinReserveAccountLedger_EmptyAccounts —
// no accounts means no freshness signal to compute; the gate-permissive
// 0 sentinel matches the [ConfigReserveBalanceReader] posture.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_EmptyAccounts(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{})
	got, err := r.MinReserveAccountLedger(context.Background(), nil, 100)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got != 0 {
		t.Errorf("min=%d want 0 (no accounts → no signal)", got)
	}
}

// TestLCMReserveBalanceReader_MinReserveAccountLedger_MissingAccount
// — when one of the supplied accounts has no observation, the
// reader returns the sentinel-wrapped error so the chain reader
// drops to the gate-permissive bypass instead of pretending the
// other accounts' freshness applies to the missing one.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_MissingAccount(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			// GA2 missing on purpose.
		},
	})
	_, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2"}, 50_001_000)
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v want wrapping ErrNoObservation", err)
	}
}
