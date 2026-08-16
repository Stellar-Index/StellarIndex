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

	// MaxAccountObservationLedger reports how far the account OBSERVER has
	// PROCESSED at-or-before asOfLedger — the true observer watermark, which
	// advances every ledger the observer runs regardless of whether any
	// watched account changed (F-1320/R-002/CS-102; storage impl reads
	// account_observer_watermark). NOT the last balance-change ledger: a quiet
	// reserve account must not read as a stalled observer.
	//
	// Deliberately part of the REQUIRED interface rather than an optional
	// one probed by type assertion (CS-102): a missing delegate behind an
	// optional interface degrades silently to the old, wrong anchor and
	// looks exactly like healthy operation. Adding a method here breaks
	// implementers at compile time instead, which is the point.
	MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error)
}

// AccountObservationRow is the storage-side shape mirrored into
// the supply package so we don't import timescale here (avoids a
// cyclic import: timescale already imports supply for InsertSupply).
// Caller (internal/ops/supply/supply.go) adapts the timescale row
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
// Returns how far the account OBSERVER has progressed at-or-before
// `asOfLedger`, provided every supplied account is actually observed.
// F-1236 (codex audit-2026-05-12): closes the third leg of the
// supply-snapshot freshness gate.
//
// CS-102 (2026-07-28), third leg. This used to return MIN(row.Ledger)
// across the accounts — each one's LAST observation. SDF reserve accounts
// move every few days-to-weeks by design, so that anchor went stale while
// nothing was wrong, the gate read a stalled observer, and XLM's served
// supply froze. Per-account last-activity measures how busy an account is,
// not whether our data is current; the observer watermark measures the
// latter, and a dead observer still stops advancing it for every account at
// once.
//
// The per-account probe is RETAINED — not for its ledger value, but because
// "every configured reserve account is actually observed" is a real
// precondition. If one is missing we cannot compute the reserve exclusion at
// all, so the gate must stay permissive rather than bless a partial sum
// behind a healthy-looking watermark.
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
