package supply

import (
	"errors"
	"fmt"
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
// bound produces false alerts there. [WrapClass] / [CrossCheckForClass]
// handle that case; this constant is the tolerance for BOTH comparison
// shapes.
var CrossCheckTolerance = big.NewInt(1)

// WrapClass classifies which invariant a [CrossCheckPair] is expected
// to satisfy. Comparing Algorithm 2's classic TOTAL supply against
// Algorithm 3's SAC-wrapped supply with a 1-stroop tolerance (as
// `stellarindex_supply_cross_check_divergence` would under a single
// equality rule) is only a true invariant when the asset is genuinely
// 100% SAC-represented. For the common case — a classic asset that
// merely HAS a SAC wrapper but is mostly held classically
// (trustlines/claimables/LP, not the SAC) — the two legitimately
// diverge by ~the whole supply (e.g. AQUA: Alg-2 ≈ 86.4B, Alg-3 ≈ 0),
// so an equality compare would raise standing false positives. Served
// supply is unaffected; only the comparison would be wrong.
//
// See [CrossCheckForClass] for what each class actually checks.
type WrapClass string

const (
	// WrapClassPartial is the default, safe classification: most of the classic
	// asset's supply is presumed to live OUTSIDE the SAC, so [CrossCheckForClass]
	// does NOT check total-vs-total equality. It checks the invariant that holds at
	// any wrap fraction: the SAC's total_supply (Algorithm 3) can never exceed the
	// classic total_supply (Algorithm 2), because SACWrapped is one of Algorithm 2's
	// non-negative addends ([ClassicSupplyComponents]). sac_total > classic_total is
	// a genuine "escrow != minted" signal; sac_total <= classic_total must not page.
	//
	// SECOND LEG: [CrossCheckSubsetBound] compares Algorithm 2's SACWrapped component
	// with Algorithm 3's total_supply (the SAME escrowed quantity by independent
	// paths), so SACWrapped > sac_total is a genuine violation. It reads
	// [Supply.SACWrappedStroops], never [ClassicSupplyStore.SumSACBalancesAtOrBefore]
	// directly, which reads at an unrelated ledger (the skew
	// [CrossCheckLedgerTolerance] bounds). A nil SACWrappedStroops leaves
	// [CrossCheckResult.SubsetBoundChecked] false: a zero default would pass
	// vacuously and publish a green check that verified nothing.
	//
	// KNOWN LIMITATION (docs/architecture/supply-pipeline.md "Dormant contract-held
	// SAC balances"): the inequality assumes Algorithm 2's total is not itself an
	// undercount. For BLND/EURC/KALE/PHO it was: their largest holders are pool
	// contracts dormant since before the ClickHouse projection's ~62M floor, which
	// the default `supply seed-sac-balances` never saw (fix: `-full-history`;
	// `sac_balance_seed_provenance` records it). Until then this check can
	// false-positive in the sac_total > classic_total direction; not corruption.
	WrapClassPartial WrapClass = "partial_wrap"

	// WrapClassFull is an operator attestation that a classic asset's
	// ENTIRE economic supply is represented through its SAC wrapper —
	// no meaningful classic-trustline circulation exists outside it.
	// Under this class total-vs-total equality (the ADR-0011
	// compare) IS a true invariant, so
	// [CrossCheckForClass] runs the strict equality check and any
	// drift beyond [CrossCheckTolerance] pages.
	//
	// Real Stellar classic assets are essentially never 100% wrapped,
	// so no pair is flagged Full by default. Flipping a pair to
	// Full is an operator config change (`[supply].fully_wrapped_sacs`)
	// and should carry the same evidence-trail discipline as a
	// WASM-history BackfillSafe flip (docs/operations/wasm-audits/).
	WrapClassFull WrapClass = "full_wrap"
)

// normalizeWrapClass maps the Go zero-value ("") — and any value other
// than [WrapClassFull] — to the safe [WrapClassPartial] default, so a
// [CrossCheckPair] built without explicitly setting WrapClass never
// accidentally lands on the stricter Full behaviour.
func normalizeWrapClass(c WrapClass) WrapClass {
	if c == WrapClassFull {
		return WrapClassFull
	}
	return WrapClassPartial
}

// CrossCheckResult is the comparison output. The caller emits the
// metric + alert based on WithinTolerance. ClassicTotal / SACTotal
// are the inputs preserved on the result so log lines and runbook
// dashboards can reproduce the comparison without re-querying.
//
// DivergenceStroops's meaning depends on WrapClass:
//   - [WrapClassFull]: |classic.TotalSupply − sac.TotalSupply| — the
//     ADR-0011 equality compare.
//   - [WrapClassPartial]: EscrowExcessStroops (leg 2), or 0 when
//     !SubsetBoundChecked. OverMintStroops (leg 1) is diagnostic-only
//     and never feeds it, so an over-mint alone cannot move the
//     `stellarindex_supply_cross_check_divergence_stroops` gauge or
//     page.
//
// Both shapes report a non-negative *big.Int and WithinTolerance=true
// when DivergenceStroops ≤ [CrossCheckTolerance].
type CrossCheckResult struct {
	ClassicKey        string
	SACKey            string
	ClassicTotal      *big.Int
	SACTotal          *big.Int
	DivergenceStroops *big.Int
	WithinTolerance   bool
	WrapClass         WrapClass

	// ClassicLedger / SACLedger are the LedgerSequence each snapshot
	// was computed at, so a reader can see the two sides are
	// contemporaneous; 0 means the snapshot recorded no ledger anchor.
	ClassicLedger uint32
	SACLedger     uint32

	// OverMintStroops is leg 1: max(0, SACTotal − ClassicTotal), the
	// cumulative-net-mint vs classic-outstanding gap. DIAGNOSTIC ONLY:
	// it can be legitimately positive (a classic-side retirement or a
	// one-time SAC mint distributed classically — see
	// [CrossCheckSubsetBound]) and does not feed DivergenceStroops or
	// WithinTolerance. Populated only under [WrapClassPartial]; nil
	// under [WrapClassFull].
	OverMintStroops *big.Int

	// SACWrapped is Algorithm 2's SACWrapped component as recorded on
	// the classic snapshot ([Supply.SACWrappedStroops]) — the input to
	// leg 2, preserved for log lines and runbook triage. nil when the
	// snapshot recorded none.
	SACWrapped *big.Int

	// SubsetBoundChecked is the disambiguator for leg 2: true
	// means a real SACWrapped component fed the escrow bound; false
	// means the classic snapshot carried none (a pre-migration-0117
	// row, or a non-classic algorithm) so the leg was NOT evaluated.
	//
	// false MUST NOT be read as "the escrow side agrees". Leg 2 is the
	// only leg that feeds DivergenceStroops, so a green WithinTolerance
	// with SubsetBoundChecked=false checked NOTHING — divergence is 0
	// by construction.
	SubsetBoundChecked bool

	// EscrowExcessStroops is leg 2: max(0, SACWrapped − SACTotal). The
	// balances escrowed inside the SAC cannot exceed what the SAC has
	// net-minted, because every escrowed unit got there by a mint. A
	// positive value means either mints the indexer never captured or
	// burns it double-counted. nil when !SubsetBoundChecked.
	EscrowExcessStroops *big.Int
}

// ErrCrossCheckNilSupply is returned by [CrossCheck] when either
// argument has a nil TotalSupply (the per-algorithm Computers always
// populate TotalSupply on success; a nil here is a caller bug).
var ErrCrossCheckNilSupply = errors.New("supply: cross-check requires non-nil TotalSupply on both inputs")

// ErrCrossCheckMisaligned is returned by [CrossCheckForClass] when the
// two snapshots were computed more than [CrossCheckLedgerTolerance]
// ledgers apart. Comparing arbitrarily-aged totals makes both
// invariants unsound in both directions, so there is no verdict to give.
var ErrCrossCheckMisaligned = errors.New("supply: cross-check snapshots describe different ledgers")

// checkLedgerAlignment refuses a pair whose snapshots are further apart
// than [CrossCheckLedgerTolerance]. A zero LedgerSequence on either side
// means "no ledger anchor recorded" (bootstrap / static-fallback
// snapshot) and takes the permissive path, mirroring the
// MinComponentLedger==0 convention the freshness gate uses.
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

// CrossCheck compares a classic-asset Algorithm 2 reading with its SAC-wrapped
// Algorithm 3 reading under the STRICT total-vs-total equality invariant
// ([WrapClassFull] semantics), correct only for a genuinely fully-SAC-represented
// asset.
//
// CAVEAT: equality holds only when the ENTIRE supply moves through the SAC's
// SEP-41 mint/burn events. A classic asset that merely HAS a SAC wrapper diverges
// by ~the whole supply (AQUA: Alg-2 ~86.4B, Alg-3 ~0), so a 1-stroop tolerance
// would fire a FALSE supply_cross_check_divergence alert. Config-driven callers
// ([CrossCheckRefresher]) go through [CrossCheckForClass], which routes
// partial-wrap pairs to [CrossCheckSubsetBound].
//
// The leg-2 fields ([CrossCheckResult.SACWrapped], SubsetBoundChecked,
// EscrowExcessStroops) and OverMintStroops stay zero on purpose: equality
// already subsumes the escrow bound (SACWrapped <= classic.TotalSupply <=
// sac.TotalSupply + [CrossCheckTolerance]).
//
// Pure: no I/O or metrics; the caller emits [obs.SupplyCrossCheckDivergence].
// Pre-conditions: both Supply values have non-nil TotalSupply, and the caller
// confirms both AssetKeys are the same asset (e.g. SAC id derived from the
// classic CODE+ISSUER); CrossCheck does not verify the pairing.
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

// CrossCheckSubsetBound compares a classic-asset Algorithm 2 reading with its
// SAC-wrapped Algorithm 3 reading under the [WrapClassPartial] invariants, in two
// legs:
//
//	leg 1 (over-mint, DIAGNOSTIC ONLY): OverMintStroops = max(0, sac - classic).
//	  Never alerting: its premise holds only for a one-way wrap (BLND / PHO).
//	leg 2 (escrow-exceeds-minted): classic.SACWrappedStroops <= sac.TotalSupply.
//	  Every unit escrowed in the SAC got there by a mint, so the ledger-entry sum
//	  of SAC balances cannot exceed the event-derived net mint (excess =
//	  EscrowExcessStroops).
//
// DivergenceStroops = EscrowExcessStroops (0 when !SubsetBoundChecked), so the
// gauge + alert fire on a leg-2 breach only. Leg 2 runs only when
// classic.SACWrappedStroops is non-nil; otherwise SubsetBoundChecked stays false,
// never defaulted to zero (vacuous pass). A caller reading WithinTolerance MUST
// read SubsetBoundChecked alongside it.
//
// STILL NOT A FULL RECONCILIATION. The NON-SAC half of Algorithm 2 (trustlines /
// claimables / LP reserves) has no independent second observation, and an
// UNDER-counted SACWrapped (the dormant-pool case in [WrapClassPartial]'s KNOWN
// LIMITATION) sits BELOW sac_total and passes quietly: leg 2 is an upper bound
// on escrow, not proof it was fully observed.
//
// Pure, same pre-conditions as [CrossCheck].
func CrossCheckSubsetBound(classic, sac Supply) (CrossCheckResult, error) {
	if classic.TotalSupply == nil || sac.TotalSupply == nil {
		return CrossCheckResult{}, ErrCrossCheckNilSupply
	}

	// Leg 1 (over-mint) is DIAGNOSTIC-ONLY: its
	// premise — sac.TotalSupply ≤ classic.TotalSupply — conflates the
	// event-derived CUMULATIVE net mint with the CURRENT escrowed
	// balance, and only holds for a one-way wrap where nothing ever
	// leaves contract space. Two live counter-examples falsified it:
	//   - BLND: 130.1M ever minted through the SAC, 4.5M SAC-burned,
	//     but ~12.6M more was later retired CLASSICALLY (paid back to
	//     the issuer — no SAC burn event fires on that path), so
	//     classic outstanding (113.0M) sits legitimately below the
	//     cumulative net mint (125.6M).
	//   - PHO: the full 200M supply was minted through the SAC once
	//     and then distributed classic-side; classic outstanding is
	//     issuer-EXCLUDED (77.9M), so the one-time cumulative mint
	//     exceeds it forever, by design.
	// Both would page as "divergence" while every unit is
	// accounted for. OverMintStroops is still computed and reported so
	// an operator can read the cumulative-vs-outstanding gap, but it
	// does not feed DivergenceStroops — leg 2 below is the bound
	// that is impossible to breach under correct accounting for a
	// partial wrap (escrow got there by mints, so escrow ≤ net mint).
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

// excessOver returns max(0, have − bound) as a fresh *big.Int: how far
// `have` breaches the upper bound `bound`, clamped at zero so a
// satisfied bound reports 0 rather than a negative gauge reading.
func excessOver(have, bound *big.Int) *big.Int {
	excess := new(big.Int).Sub(have, bound)
	if excess.Sign() < 0 {
		return big.NewInt(0)
	}
	return excess
}

// CrossCheckForClass dispatches to [CrossCheck] (equality) or
// [CrossCheckSubsetBound] (subset bound) based on class, normalizing
// the zero-value / any unrecognized class to [WrapClassPartial] — the
// safe default — via [normalizeWrapClass]. It first refuses a
// misaligned pair with [ErrCrossCheckMisaligned], so the aggregator's
// [CrossCheckRefresher] and the `supply audit -cross-check` CLI share
// one ledger-alignment guard. CrossCheck / CrossCheckSubsetBound stay exported
// for direct unit testing of the pure comparisons.
func CrossCheckForClass(classic, sac Supply, class WrapClass) (CrossCheckResult, error) {
	if err := checkLedgerAlignment(classic, sac); err != nil {
		return CrossCheckResult{}, err
	}
	if normalizeWrapClass(class) == WrapClassFull {
		return CrossCheck(classic, sac)
	}
	return CrossCheckSubsetBound(classic, sac)
}
