package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
)

// AccountObservationLookup is the storage-side primitive the
// [LCMReserveBalanceReader] consumes. Production impl is
// timescale.Store.LatestAccountObservationAtOrBefore; tests pass
// in-memory fakes.
//
// Returns an "observation not found" sentinel when the account
// has no observation at-or-before the requested ledger — the
// reader translates that into [ErrNoObservation] so the chained-
// fallback caller can drop to the config reader.
type AccountObservationLookup interface {
	LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (AccountObservationRow, error)
}

// AccountObservationRow is the storage-side shape mirrored into
// the supply package so we don't import timescale here (avoids a
// cyclic import: timescale already imports supply for InsertSupply).
// Caller (cmd/stellaratlas-ops/supply.go) adapts the timescale row
// into this shape.
type AccountObservationRow struct {
	Balance   *big.Int
	IsRemoval bool
	Ledger    uint32
}

// ErrNoObservation is returned by [LCMReserveBalanceReader] when
// at least one configured reserve account has no observation
// at-or-before the requested ledger. The caller (typically the
// supply-snapshot subcommand) treats this as the "live data not
// available yet, fall back to operator-static config" signal —
// not a hard error.
var ErrNoObservation = errors.New("supply: no LCM observation for at least one reserve account")

// LCMReserveBalanceReader is a [ReserveBalanceReader] backed by
// the LCM-derived `account_observations` hypertable. Replaces the
// operator-static [ConfigReserveBalanceReader] (#285) once the
// AccountEntry observer (#298) has been backfilled to a deep enough
// range.
//
// Per ADR-0021 the static reader stays in tree as a bootstrap
// fallback. Operators that deploy the LCM reader without a
// backfilled observer get [ErrNoObservation] until the observer
// catches up; the supply-snapshot subcommand uses the chained
// fallback pattern (try LCM first, fall back to config) so the
// transition is seamless.
type LCMReserveBalanceReader struct {
	store AccountObservationLookup
}

// NewLCMReserveBalanceReader constructs the live reader. `store`
// is typically a *timescale.Store via an adapter that maps the
// timescale.AccountObservation row into [AccountObservationRow].
func NewLCMReserveBalanceReader(store AccountObservationLookup) *LCMReserveBalanceReader {
	return &LCMReserveBalanceReader{store: store}
}

// ReserveBalanceTotal sums the latest observed balance for each
// account in `accounts` at-or-before `ledger`. Returns
// [ErrNoObservation] when any account has no observation in
// scope — the caller's chained-fallback path drops to the static
// config reader for the whole call (we don't mix live + static
// per call; that would silently produce a partially-fresh sum
// the operator can't audit).
//
// Removed-account observations (IsRemoval=true) yield a balance
// of zero in the sum, consistent with the on-chain post-state
// (the AccountEntry no longer exists).
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

// MinReserveAccountLedger implements [ReserveBalanceFreshnessReader].
// Returns MIN(row.Ledger) across the supplied accounts at-or-before
// `asOfLedger`. F-1236 (codex audit-2026-05-12): closes the third
// leg of the supply-snapshot freshness gate.
//
// Removed-account observations contribute their removal ledger
// (the AccountEntry-delete is the most-recent observation we
// have for that key, so it IS the freshness signal — pretending
// the removal didn't happen would over-state freshness for an
// account that no longer exists).
//
// Returns 0 (gate-permissive bypass) when:
//   - `accounts` is empty (no signal to compute);
//   - any account has no observation at-or-before `asOfLedger`
//     (indistinguishable from "the observer hasn't backfilled
//     this account yet"); same shape the
//     [ConfigReserveBalanceReader] preserves by not implementing
//     this interface at all.
//
// Returns a non-nil error only on storage-side failures the
// caller should bubble; the [XLMComputer] swallows them and
// falls back to the legacy permissive posture so a transient
// query error doesn't reject an otherwise-valid snapshot.
func (r *LCMReserveBalanceReader) MinReserveAccountLedger(ctx context.Context, accounts []string, asOfLedger uint32) (uint32, error) {
	if len(accounts) == 0 {
		return 0, nil
	}
	var minLedger uint32
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
		if minLedger == 0 || row.Ledger < minLedger {
			minLedger = row.Ledger
		}
	}
	return minLedger, nil
}

// Compile-time checks.
var (
	_ ReserveBalanceReader          = (*LCMReserveBalanceReader)(nil)
	_ ReserveBalanceFreshnessReader = (*LCMReserveBalanceReader)(nil)
)
