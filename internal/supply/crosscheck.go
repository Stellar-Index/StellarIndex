package supply

import (
	"errors"
	"math/big"
)

// CrossCheckTolerance is the maximum acceptable difference between a
// classic-asset's Algorithm 2 total_supply and its SAC-wrapped form's
// Algorithm 3 total_supply, in stroops. Per ADR-0011: "Cross-check:
// alert when they disagree by more than 1 stroop."
//
// One stroop is the float-rounding boundary that arises in honest
// indexer math (a NUMERIC truncation here, a Soroban-emitted i128 →
// SAC contract-data write rounding there). Anything larger is a real
// disagreement worth paging — BUT only for a fully-SAC-represented
// asset; see the CAVEAT on [CrossCheck]: a partially-wrapped classic
// asset legitimately diverges by ~its whole supply, so this 1-stroop
// bound produces false alerts there (BACKLOG #59).
var CrossCheckTolerance = big.NewInt(1)

// CrossCheckResult is the comparison output. The caller emits the
// metric + alert based on WithinTolerance. ClassicTotal / SACTotal
// are the inputs preserved on the result so log lines and runbook
// dashboards can reproduce the comparison without re-querying.
//
// DivergenceStroops is |classic.TotalSupply − sac.TotalSupply| as a
// non-negative *big.Int. Equal totals report DivergenceStroops=0 and
// WithinTolerance=true.
type CrossCheckResult struct {
	ClassicKey        string
	SACKey            string
	ClassicTotal      *big.Int
	SACTotal          *big.Int
	DivergenceStroops *big.Int
	WithinTolerance   bool
}

// ErrCrossCheckNilSupply is returned by [CrossCheck] when either
// argument has a nil TotalSupply (the per-algorithm Computers always
// populate TotalSupply on success; a nil here is a caller bug).
var ErrCrossCheckNilSupply = errors.New("supply: cross-check requires non-nil TotalSupply on both inputs")

// CrossCheck compares a classic-asset Algorithm 2 reading with its
// SAC-wrapped Algorithm 3 reading.
//
// CAVEAT (audit-2026-07-07, see BACKLOG #59): the equality
// classic.TotalSupply == sac.TotalSupply only holds for an asset whose
// ENTIRE economic supply is represented through the SAC's SEP-41
// mint/burn events (a genuinely SAC-issued token). It does NOT hold for
// a classic asset that merely HAS a SAC wrapper but is mostly held
// classically: Algorithm 2 sums the TOTAL classic supply (trustlines +
// claimables + LP + contract balances) while Algorithm 3 sums only the
// SEP-41-MINTED amount — which is ~0 for a classic asset that the
// classic issuer mints (not the SAC). For such assets the two legitimately
// diverge by ~the whole supply (e.g. AQUA: Alg-2 ≈ 86.4B, Alg-3 ≈ 0), so a
// 1-stroop tolerance fires a FALSE supply_cross_check_divergence alert.
// The comparison is only a valid corruption signal for fully-SAC-
// represented assets; #59 tracks fixing the pair selection / comparison.
// Until then, over-tolerance divergence on a classic asset is a category
// error, not indexer corruption — do NOT treat it as a served-supply bug
// (the served total, via Algorithm 2, is correct).
//
// The function is pure: no I/O, no metric emission. The caller emits
// metrics via [obs.SupplyCrossCheckDivergence] using the returned
// result. Keeping CrossCheck pure lets unit tests cover the
// comparison without a Prometheus dependency.
//
// Pre-conditions:
//   - Both Supply values must have non-nil TotalSupply.
//   - Caller is responsible for confirming the two AssetKeys refer
//     to the same underlying asset (e.g. by deriving the SAC contract
//     id from the classic asset's CODE+ISSUER). CrossCheck does NOT
//     verify the pairing — there's no on-chain way to do so without
//     re-deriving the SAC address upstream, which the caller is
//     better positioned to handle.
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
	}, nil
}
