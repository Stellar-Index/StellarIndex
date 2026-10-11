package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
)

// AccountObservationLookup is the storage-side primitive of [LCMReserveBalanceReader]. A missing
// observation maps to [ErrNoObservation] so the chained caller falls back to config.
type AccountObservationLookup interface {
	LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (AccountObservationRow, error)

	// MaxAccountObservationLedger reports the account OBSERVER's processed watermark at-or-before
	// asOfLedger (account_observer_watermark), not the last balance change: a quiet reserve account
	// must not read as a stalled observer. Required rather than an optional interface probed by type
	// assertion, which would degrade silently to a wrong anchor.
	MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error)
}

// AccountObservationRow mirrors the timescale row so supply doesn't import timescale (which
// imports supply).
type AccountObservationRow struct {
	Balance   *big.Int
	IsRemoval bool
	Ledger    uint32
}

// ErrNoObservation means a configured reserve account has no observation at-or-before the
// requested ledger; callers fall back to static config rather than failing.
var ErrNoObservation = errors.New("supply: no LCM observation for at least one reserve account")

// LCMReserveBalanceReader is a [ReserveBalanceReader] over the account_observations hypertable.
// It supersedes [ConfigReserveBalanceReader] once the observer is backfilled (ADR-0021); until
// then it returns [ErrNoObservation] and the chained reader falls back to config.
type LCMReserveBalanceReader struct {
	store AccountObservationLookup
}

// NewLCMReserveBalanceReader constructs the live reader. `store`
// is typically a *timescale.Store via an adapter that maps the
// timescale.AccountObservation row into [AccountObservationRow].
func NewLCMReserveBalanceReader(store AccountObservationLookup) *LCMReserveBalanceReader {
	return &LCMReserveBalanceReader{store: store}
}

// ReserveBalanceTotal sums the latest observed balance per account at-or-before `ledger`. Any
// missing observation returns [ErrNoObservation] for the whole call: live and static are never
// mixed, since the partial sum would be unauditable. Removed accounts count as zero.
func (r *LCMReserveBalanceReader) ReserveBalanceTotal(ctx context.Context, accounts []string, ledger uint32) (*big.Int, error) {
	total := big.NewInt(0)
	for _, acc := range accounts {
		row, err := r.store.LatestAccountObservationAtOrBefore(ctx, acc, ledger)
		if err != nil {
			return nil, fmt.Errorf("%w: account %s: %w", ErrNoObservation, acc, err)
		}
		if row.IsRemoval {
			// Removed account contributes 0; skip the Add (avoid a
			// nil-Balance deref in the unlikely case the storage
			// adapter forgot to zero it).
			continue
		}
		if row.Balance == nil {
			return nil, fmt.Errorf("%w: account %s: nil Balance from store", ErrNoObservation, acc)
		}
		total = new(big.Int).Add(total, row.Balance)
	}
	return total, nil
}

// MinReserveAccountLedger implements [ReserveBalanceFreshnessReader] with the observer watermark,
// not MIN over per-account observations: SDF reserve accounts move rarely, so a per-account
// anchor would go stale and freeze XLM supply. Returns 0 (gate-permissive) when accounts is empty
// or any account is unobserved, so a partial sum never passes behind a healthy watermark.
func (r *LCMReserveBalanceReader) MinReserveAccountLedger(ctx context.Context, accounts []string, asOfLedger uint32) (uint32, error) {
	if len(accounts) == 0 {
		return 0, nil
	}
	for _, acc := range accounts {
		row, err := r.store.LatestAccountObservationAtOrBefore(ctx, acc, asOfLedger)
		if err != nil {
			// Treat lookup failure as "no signal" rather than
			// surfacing — caller (XLMComputer) treats this as
			// the gate-permissive bypass.
			return 0, fmt.Errorf("%w: account %s: %w", ErrNoObservation, acc, err)
		}
		if row.Ledger == 0 {
			// Sentinel "no observation found" — the gate sees
			// this as no-signal across the set.
			return 0, nil
		}
	}
	return r.store.MaxAccountObservationLedger(ctx, asOfLedger)
}

// Compile-time checks.
var (
	_ ReserveBalanceReader          = (*LCMReserveBalanceReader)(nil)
	_ ReserveBalanceFreshnessReader = (*LCMReserveBalanceReader)(nil)
)
