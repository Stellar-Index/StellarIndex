package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
)

// ReserveSource names which reader actually produced a reserve-balance
// total, so the published snapshot can say where its number came from.
type ReserveSource string

const (
	// ReserveSourceLive — per-account LCM observations at the ledger.
	ReserveSourceLive ReserveSource = "live"
	// ReserveSourceStatic — the operator's dated balance map.
	ReserveSourceStatic ReserveSource = "static"
)

// ReserveBalanceSourcedReader is the optional extension to
// [ReserveBalanceReader] that also reports which source answered. The
// [XLMComputer] uses it to label a static-map answer with
// [BasisXLMSDFReserveExclusionStatic]; a reader that does not
// implement it is treated as live.
type ReserveBalanceSourcedReader interface {
	ReserveBalanceTotalSourced(ctx context.Context, accounts []string, ledger uint32) (*big.Int, ReserveSource, error)
}

// ChainedReserveBalanceReader tries the live reader first and falls
// back to the static reader for the WHOLE call when the live reader
// returns [ErrNoObservation] (ADR-0021: live and static are never
// mixed within one sum). Any other live error, and any static error,
// is returned. The aggregator refresher and the `supply snapshot` CLI
// share this one implementation so both label the static arm.
type ChainedReserveBalanceReader struct {
	live   ReserveBalanceReader
	static ReserveBalanceReader
}

// NewChainedReserveBalanceReader composes live over static.
func NewChainedReserveBalanceReader(live, static ReserveBalanceReader) *ChainedReserveBalanceReader {
	return &ChainedReserveBalanceReader{live: live, static: static}
}

// ReserveBalanceTotal implements [ReserveBalanceReader].
func (c *ChainedReserveBalanceReader) ReserveBalanceTotal(ctx context.Context, accounts []string, ledger uint32) (*big.Int, error) {
	v, _, err := c.ReserveBalanceTotalSourced(ctx, accounts, ledger)
	return v, err
}

// ReserveBalanceTotalSourced implements [ReserveBalanceSourcedReader].
func (c *ChainedReserveBalanceReader) ReserveBalanceTotalSourced(ctx context.Context, accounts []string, ledger uint32) (*big.Int, ReserveSource, error) {
	out, err := c.live.ReserveBalanceTotal(ctx, accounts, ledger)
	if err == nil {
		return out, ReserveSourceLive, nil
	}
	if !errors.Is(err, ErrNoObservation) {
		return nil, "", err
	}
	// The static reader's own errors (missing account, expired snapshot)
	// are operator-config conditions, so they bubble as themselves; the
	// live cause is kept as text only so the refresher classes the tick
	// by the static failure, not as a benign no_observation.
	out, serr := c.static.ReserveBalanceTotal(ctx, accounts, ledger)
	if serr != nil {
		return nil, "", fmt.Errorf("supply: live reserve read fell through (%v); static fallback: %w", err, serr)
	}
	return out, ReserveSourceStatic, nil
}

// MinReserveAccountLedger forwards the freshness probe to the live
// reader. The [XLMComputer] only asks when the balance came from the
// live arm; a live [ErrNoObservation] is the gate-permissive 0.
func (c *ChainedReserveBalanceReader) MinReserveAccountLedger(ctx context.Context, accounts []string, ledger uint32) (uint32, error) {
	fr, ok := c.live.(ReserveBalanceFreshnessReader)
	if !ok {
		return 0, nil
	}
	got, err := fr.MinReserveAccountLedger(ctx, accounts, ledger)
	if errors.Is(err, ErrNoObservation) {
		return 0, nil
	}
	return got, err
}

var (
	_ ReserveBalanceSourcedReader   = (*ChainedReserveBalanceReader)(nil)
	_ ReserveBalanceFreshnessReader = (*ChainedReserveBalanceReader)(nil)
	_ ReserveBalanceSourcedReader   = (*ConfigReserveBalanceReader)(nil)
)
