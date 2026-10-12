package supply

import (
	"errors"
	"fmt"
	"math/big"
)

// CrossCheckTolerance is the maximum classic-vs-SAC total_supply difference in stroops (ADR-0011: alert beyond 1 stroop), paging only for a fully-SAC-represented asset (see [CrossCheck]).
var CrossCheckTolerance = big.NewInt(1)

// WrapClass classifies which invariant a [CrossCheckPair] must satisfy: strict equality holds only for an asset 100% SAC-represented, while one that merely HAS a SAC wrapper legitimately diverges.
type WrapClass string

const (
	// WrapClassPartial is the safe default: [CrossCheckForClass] checks SAC total (Alg 3) <= classic
	// total (Alg 2) plus the escrow leg from [Supply.SACWrappedStroops]. KNOWN LIMITATION
	// (docs/architecture/supply-pipeline.md "Dormant contract-held SAC balances"): undercounted Alg 2 can false-positive.
	WrapClassPartial WrapClass = "partial_wrap"

	// WrapClassFull is an operator attestation (`[supply].fully_wrapped_sacs`) that the asset's ENTIRE
	// supply is represented through its SAC, so total-vs-total equality (ADR-0011) is a true invariant.
	// None is Full by default; flipping one needs evidence (docs/operations/wasm-audits/).
	WrapClassFull WrapClass = "full_wrap"
)

// normalizeWrapClass maps the zero value and anything but [WrapClassFull] to [WrapClassPartial],
// so an unset WrapClass never lands on the stricter Full behaviour.
func normalizeWrapClass(c WrapClass) WrapClass {
	if c == WrapClassFull {
		return WrapClassFull
	}
	return WrapClassPartial
}

// CrossCheckResult is the comparison output the caller alerts on via WithinTolerance; DivergenceStroops is non-negative, and OverMintStroops never feeds it.
type CrossCheckResult struct {
	ClassicKey        string
	SACKey            string
	ClassicTotal      *big.Int
	SACTotal          *big.Int
	DivergenceStroops *big.Int
	WithinTolerance   bool
	WrapClass         WrapClass

	// ClassicLedger / SACLedger are the ledgers each snapshot was computed at, showing the two
	// sides are contemporaneous; 0 means no ledger anchor was recorded.
	ClassicLedger uint32
	SACLedger     uint32

	// OverMintStroops is leg 1: max(0, SACTotal - ClassicTotal). DIAGNOSTIC ONLY: it can be
	// legitimately positive (see [CrossCheckSubsetBound]) and feeds neither DivergenceStroops nor
	// WithinTolerance. Set under [WrapClassPartial]; nil under [WrapClassFull].
	OverMintStroops *big.Int

	// SACWrapped is the classic snapshot's SACWrapped component ([Supply.SACWrappedStroops]), the
	// input to leg 2, kept for triage; nil when the snapshot recorded none.
	SACWrapped *big.Int

	// SubsetBoundChecked is true when a real SACWrapped component fed leg 2, false when the classic
	// snapshot carried none. false MUST NOT be read as "the escrow side agrees": leg 2 alone feeds
	// DivergenceStroops, so a green WithinTolerance then checked NOTHING.
	SubsetBoundChecked bool

	// EscrowExcessStroops is leg 2: max(0, SACWrapped - SACTotal). Escrowed units got there by a mint,
	// so a positive value means uncaptured mints or double-counted burns. nil when !SubsetBoundChecked.
	EscrowExcessStroops *big.Int
}

// ErrCrossCheckNilSupply is returned by [CrossCheck] when either TotalSupply is nil, a caller bug.
var ErrCrossCheckNilSupply = errors.New("supply: cross-check requires non-nil TotalSupply on both inputs")

// ErrCrossCheckMisaligned is returned by [CrossCheckForClass] when the snapshots are more than [CrossCheckLedgerTolerance] ledgers apart, since totals of different ages make both invariants unsound.
var ErrCrossCheckMisaligned = errors.New("supply: cross-check snapshots describe different ledgers")

// checkLedgerAlignment refuses a pair further apart than [CrossCheckLedgerTolerance]. A zero
// LedgerSequence on either side means no anchor recorded and passes.
func checkLedgerAlignment(classic, sac Supply) error {
	if classic.LedgerSequence == 0 || sac.LedgerSequence == 0 {
		return nil
	}
	if gap := ledgerGap(classic.LedgerSequence, sac.LedgerSequence); gap > CrossCheckLedgerTolerance {
		return fmt.Errorf("%w: %d ledgers apart (classic=%d sac=%d), tolerance %d",
			ErrCrossCheckMisaligned, gap, classic.LedgerSequence, sac.LedgerSequence, CrossCheckLedgerTolerance)
	}
	return nil
}

// CrossCheck compares Alg 2 and Alg 3 under strict total-vs-total equality ([WrapClassFull]); CAVEAT: an asset that merely has a SAC wrapper diverges by its whole supply, so config-driven callers use [CrossCheckForClass].
func CrossCheck(classic, sac Supply) (CrossCheckResult, error) {
	if classic.TotalSupply == nil || sac.TotalSupply == nil {
		return CrossCheckResult{}, ErrCrossCheckNilSupply
	}

	delta := new(big.Int).Sub(classic.TotalSupply, sac.TotalSupply)
	abs := new(big.Int).Abs(delta)

	return CrossCheckResult{
		ClassicKey:        classic.AssetKey,
		SACKey:            sac.AssetKey,
		ClassicTotal:      new(big.Int).Set(classic.TotalSupply),
		SACTotal:          new(big.Int).Set(sac.TotalSupply),
		DivergenceStroops: abs,
		WithinTolerance:   abs.Cmp(CrossCheckTolerance) <= 0,
		WrapClass:         WrapClassFull,
		ClassicLedger:     classic.LedgerSequence,
		SACLedger:         sac.LedgerSequence,
	}, nil
}

// CrossCheckSubsetBound compares Alg 2 and 3 under [WrapClassPartial]: leg 2 (classic.SACWrappedStroops <= sac.TotalSupply) sets DivergenceStroops (0 when !SubsetBoundChecked), leg 1 is diagnostic only, and under-counted SACWrapped passes.
func CrossCheckSubsetBound(classic, sac Supply) (CrossCheckResult, error) {
	if classic.TotalSupply == nil || sac.TotalSupply == nil {
		return CrossCheckResult{}, ErrCrossCheckNilSupply
	}

	// Leg 1 is DIAGNOSTIC-ONLY: sac.TotalSupply <= classic.TotalSupply conflates the CUMULATIVE net
	// mint with the CURRENT escrow, so assets retired classically (BLND) or minted once via the SAC
	// then distributed classic-side (PHO) would page with every unit accounted for.
	overMint := excessOver(sac.TotalSupply, classic.TotalSupply)
	res := CrossCheckResult{
		ClassicKey:        classic.AssetKey,
		SACKey:            sac.AssetKey,
		ClassicTotal:      new(big.Int).Set(classic.TotalSupply),
		SACTotal:          new(big.Int).Set(sac.TotalSupply),
		OverMintStroops:   overMint,
		DivergenceStroops: big.NewInt(0),
		WrapClass:         WrapClassPartial,
		ClassicLedger:     classic.LedgerSequence,
		SACLedger:         sac.LedgerSequence,
	}

	if classic.SACWrappedStroops != nil {
		escrowExcess := excessOver(classic.SACWrappedStroops, sac.TotalSupply)
		res.SACWrapped = new(big.Int).Set(classic.SACWrappedStroops)
		res.SubsetBoundChecked = true
		res.EscrowExcessStroops = escrowExcess
		if escrowExcess.Cmp(res.DivergenceStroops) > 0 {
			res.DivergenceStroops = new(big.Int).Set(escrowExcess)
		}
	}

	res.WithinTolerance = res.DivergenceStroops.Cmp(CrossCheckTolerance) <= 0
	return res, nil
}

// excessOver returns max(0, have − bound) as a fresh *big.Int, clamped so a satisfied bound
// reports 0 rather than a negative gauge reading.
func excessOver(have, bound *big.Int) *big.Int {
	excess := new(big.Int).Sub(have, bound)
	if excess.Sign() < 0 {
		return big.NewInt(0)
	}
	return excess
}

// CrossCheckForClass dispatches to [CrossCheck] or [CrossCheckSubsetBound] by class (zero or unknown is [WrapClassPartial]) after refusing a misaligned pair with [ErrCrossCheckMisaligned].
func CrossCheckForClass(classic, sac Supply, class WrapClass) (CrossCheckResult, error) {
	if err := checkLedgerAlignment(classic, sac); err != nil {
		return CrossCheckResult{}, err
	}
	if normalizeWrapClass(class) == WrapClassFull {
		return CrossCheck(classic, sac)
	}
	return CrossCheckSubsetBound(classic, sac)
}
