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

	// watermark is what the account OBSERVER has reached across all accounts, the freshness anchor; deliberately distinct
	// from any row's Ledger, since the tests assert the anchor tracks this and NOT the per-account minimum.
	watermark uint32
}

func (f *fakeLookup) MaxAccountObservationLedger(_ context.Context, _ uint32) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.watermark, nil
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

// When any account has no observation the reader returns ErrNoObservation; the caller's chained-fallback handler keys on
// errors.Is(err, ErrNoObservation) to drop to the static path.
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

// A removed account contributes 0 to the total, matching on-chain post-state (the AccountEntry no longer exists, so its
// balance is 0).
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

// TestLCMReserveBalanceReader_StoreError — a generic storage error (not "not found") still
// returns wrapped in ErrNoObservation so the caller falls back to static config; the fallback
// is the same for a transient DB error and a missing row.
func TestLCMReserveBalanceReader_StoreError(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		err: errors.New("connection refused"),
	})
	_, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1"}, 100)
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v want wrapping ErrNoObservation", err)
	}
}

// The anchor is the account OBSERVER's watermark, not the oldest per-account observation (49,999,500 here): SDF reserve
// accounts move only every few days to weeks, so the per-account minimum goes stale while nothing is wrong, the gate
// reads a stalled observer, and XLM supply freezes.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_HappyPath(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			"GA2": {Balance: big.NewInt(200), Ledger: 49_999_500},
			"GA3": {Balance: big.NewInt(300), Ledger: 50_000_100},
		},
		watermark: 50_000_900,
	})
	got, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2", "GA3"}, 50_001_000)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got == 49_999_500 {
		t.Fatal("anchor regressed to the per-account MINIMUM — a quiet reserve " +
			"account is not a stalled observer, and this freezes XLM supply")
	}
	if got != 50_000_900 {
		t.Errorf("anchor=%d want 50_000_900 (observer watermark)", got)
	}
}

// A removed account still COUNTS as observed (an AccountEntry delete is a real observation), so it does not force the
// gate-permissive bypass, but its ledger does not set the anchor; the observer watermark does, so a long-ago removal
// cannot drag XLM freshness backwards.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_RemovalCounts(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			"GA2": {IsRemoval: true, Ledger: 49_990_000},
		},
		watermark: 50_000_900,
	})
	got, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2"}, 50_001_000)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got != 50_000_900 {
		t.Errorf("anchor=%d want 50_000_900 (watermark); a removed account must "+
			"not pin freshness to its removal ledger forever", got)
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

// TestLCMReserveBalanceReader_MinReserveAccountLedger_MissingAccount — when a supplied account
// has no observation, the reader returns the sentinel-wrapped error so the chain reader drops
// to the gate-permissive bypass instead of applying the other accounts' freshness to it.
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
