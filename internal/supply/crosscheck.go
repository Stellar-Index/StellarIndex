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
// bound produces false alerts there. [WrapClass] / [CrossCheckForClass]
// (2026-07-08 decision, BACKLOG #59) is the fix — this constant is
// reused unchanged as the tolerance for BOTH comparison shapes.
var CrossCheckTolerance = big.NewInt(1)

// WrapClass classifies which invariant a [CrossCheckPair] is expected
// to satisfy. Introduced 2026-07-08 (BACKLOG #59, signed off same
// day) to fix a monitoring category error:
// `stellarindex_supply_cross_check_divergence` was comparing Algorithm
// 2's classic TOTAL supply against Algorithm 3's SAC-wrapped supply
// with a 1-stroop tolerance, which is only a true invariant when the
// asset is genuinely 100% SAC-represented. For the common case — a
// classic asset that merely HAS a SAC wrapper but is mostly held
// classically (trustlines/claimables/LP, not the SAC) — the two
// legitimately diverge by ~the whole supply (e.g. AQUA: Alg-2 ≈
// 86.4B, Alg-3 ≈ 0), so the equality compare fired 8 standing false
// positives. Served supply itself was never wrong; only the
// comparison was.
//
// See [CrossCheckForClass] for what each class actually checks.
type WrapClass string

const (
	// WrapClassPartial is the default, safe classification: most of
	// the classic asset's economic supply is presumed to live OUTSIDE
	// the SAC. Under this class [CrossCheckForClass] does NOT check
	// total-vs-total equality (that would be the category error this
	// type exists to fix). Instead it checks the one invariant that
	// DOES provably hold regardless of wrap fraction: the SAC's
	// total_supply (Algorithm 3 — the wrapped amount) can never
	// exceed the classic asset's total_supply (Algorithm 2), because
	// Algorithm 2's total is Trustline + Claimable + LPReserve +
	// SACWrapped (see [ClassicSupplyComponents]) — SACWrapped is one
	// of four non-negative addends, so it is definitionally ≤ the
	// sum. sac_total > classic_total is therefore impossible under
	// correct accounting and IS a genuine corruption signal (an
	// "escrow != minted" violation) worth alerting on; sac_total ≤
	// classic_total is the expected, unremarkable case for a
	// partially-wrapped asset and must not page.
	//
	// SECOND LEG, added 2026-07-25 (audit E4/N-F3(b) — the "(b)
	// follow-up" of BACKLOG #59, formerly listed here as unbuilt):
	// [CrossCheckSubsetBound] now ALSO checks the true subset compare —
	// Algorithm 2's SACWrapped component vs Algorithm 3's total_supply.
	// Both measure the SAME quantity (the amount escrowed inside the
	// SAC) via independent data paths — a ledger-entry snapshot sum
	// (`sac_balance_observations`) vs an event-flow sum — so
	// SACWrapped > sac_total is impossible under correct accounting and
	// is a genuine escrow-exceeds-minted violation.
	//
	// The plumbing this comment used to describe as missing is option
	// (1), now built: `asset_supply_history.sac_wrapped_stroops`
	// (migration 0117) + [Supply.SACWrappedStroops] +
	// [ClassicComputer.Compute] populating it from
	// [ClassicSupplyComponents.SACWrapped]. Option (2) — the refresher
	// querying [ClassicSupplyStore.SumSACBalancesAtOrBefore] directly —
	// was NOT taken: it would re-read a component at a ledger unrelated
	// to the snapshot's own, re-introducing exactly the freshness skew
	// [CrossCheckLedgerTolerance] exists to bound, and would give the
	// aggregator a second, divergent read path to the same number.
	//
	// The leg is CS-087-gated: a snapshot with a nil SACWrappedStroops
	// (a pre-0117 row, or a non-classic algorithm that has no such
	// component) leaves [CrossCheckResult.SubsetBoundChecked] false and
	// the leg UNEVALUATED. It is deliberately not defaulted to zero —
	// 0 <= sac_total holds vacuously, so a zero default would publish a
	// green check that verified nothing.
	//
	// KNOWN LIMITATION (2026-07-10 update — see
	// docs/architecture/supply-pipeline.md "Dormant contract-held SAC
	// balances" for the full trail): this inequality assumes Algorithm
	// 2's total is not itself an undercount. BLND/EURC/KALE/PHO were a
	// documented case where it was: their largest holders are Phoenix/
	// Blend POOL CONTRACTS that acquired the SAC-wrapped token years
	// before the ClickHouse current-state projection's ~62M coverage
	// floor existed and have been dormant (no further Balance-key
	// writes) since, so `supply seed-sac-balances`'s default
	// current-state read never saw them. An earlier hypothesis here
	// guessed the balances instead lived in pool-internal, non-SEP-41
	// `contract_data` keys (needing protocol-specific decoders); the
	// 2026-07-06 final verdict superseded that — they ARE ordinary
	// `Vec(Symbol("Balance"), Address(pool))` entries on the SAC's OWN
	// storage, identical in shape to every other holder. The fix is
	// `supply seed-sac-balances -full-history`
	// (clickhouse.StreamSACBalanceSeedsFullHistory), which reads
	// stellar.ledger_entry_changes — complete to genesis (ADR-0034) —
	// instead of the floor-limited current-state projection; per-contract
	// seed provenance (source + holder count + ledger bounds) is recorded
	// in `sac_balance_seed_provenance` (migration 0102) so an operator
	// can see whether a residual divergence for a pair is "expected,
	// never full-history seeded" or "actually anomalous, already
	// full-history seeded". Until a pair IS full-history seeded, this
	// check can still false-positive in the sac_total > classic_total
	// direction for it — not a regression introduced here (the OLD
	// equality check was already broken for such a pair too) but worth
	// naming so an operator doesn't mistake a not-yet-seeded pair's
	// divergence for corruption.
	WrapClassPartial WrapClass = "partial_wrap"

	// WrapClassFull is an operator attestation that a classic asset's
	// ENTIRE economic supply is represented through its SAC wrapper —
	// no meaningful classic-trustline circulation exists outside it.
	// Under this class total-vs-total equality (the original
	// ADR-0011 compare, unchanged) IS a true invariant, so
	// [CrossCheckForClass] runs the strict equality check and any
	// drift beyond [CrossCheckTolerance] pages exactly as before.
	//
	// No pair is flagged Full as of 2026-07-08 — real Stellar classic
	// assets are essentially never 100% wrapped. Flipping a pair to
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
//     original ADR-0011 equality compare.
//   - [WrapClassPartial]: max(OverMintStroops, EscrowExcessStroops) —
//     the worse of the two impossibility bounds below. Zero in the
//     expected state for a partially-wrapped asset; positive only when
//     one of the two conservation bounds is actually breached.
//
// Both shapes report a non-negative *big.Int and WithinTolerance=true
// when DivergenceStroops ≤ [CrossCheckTolerance]. Keeping the single
// gauge as the max of the legs means the existing
// `stellarindex_supply_cross_check_divergence_stroops` alert fires on
// EITHER breach with no threshold change; the per-leg fields say which.
type CrossCheckResult struct {
	ClassicKey        string
	SACKey            string
	ClassicTotal      *big.Int
	SACTotal          *big.Int
	DivergenceStroops *big.Int
	WithinTolerance   bool
	WrapClass         WrapClass

	// OverMintStroops is leg 1: max(0, SACTotal − ClassicTotal). The
	// SAC cannot have minted more than the classic asset's whole total
	// supply, because that total already includes the escrowed amount
	// as one of its four non-negative addends. Populated only under
	// [WrapClassPartial] (the [WrapClassFull] equality compare reports
	// a signed-magnitude in DivergenceStroops instead and leaves this
	// nil).
	OverMintStroops *big.Int

	// SACWrapped is Algorithm 2's SACWrapped component as recorded on
	// the classic snapshot ([Supply.SACWrappedStroops]) — the input to
	// leg 2, preserved for log lines and runbook triage. nil when the
	// snapshot recorded none.
	SACWrapped *big.Int

	// SubsetBoundChecked is the CS-087 disambiguator for leg 2: true
	// means a real SACWrapped component fed the escrow bound; false
	// means the classic snapshot carried none (a pre-migration-0117
	// row, or a non-classic algorithm) so the leg was NOT evaluated.
	//
	// false MUST NOT be read as "the escrow side agrees". A green
	// WithinTolerance with SubsetBoundChecked=false means only "the SAC
	// has not over-minted past the classic total" — the pre-2026-07-25
	// one-sided claim — not that the two sides reconcile.
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

// CrossCheck compares a classic-asset Algorithm 2 reading with its
// SAC-wrapped Algorithm 3 reading under the STRICT total-vs-total
// equality invariant — i.e. [WrapClassFull] semantics. Equivalent to
// `CrossCheckForClass(classic, sac, WrapClassFull)`; kept as a
// standalone function (rather than folded into CrossCheckForClass)
// because it predates [WrapClass] and remains the correct, unqualified
// comparison for a genuinely-fully-SAC-represented asset.
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
// 1-stroop tolerance on THIS function fires a FALSE
// supply_cross_check_divergence alert.
//
// As of the 2026-07-08 decision, callers driven by operator config
// (the aggregator's [CrossCheckRefresher]) do NOT call CrossCheck
// directly for a partially-wrapped pair — they call
// [CrossCheckForClass] with the pair's [WrapClass], which routes
// partial-wrap pairs to [CrossCheckSubsetBound] instead. CrossCheck
// itself is unchanged and remains correct for its documented
// pre-condition (a fully-SAC-represented asset); it is exported
// directly for tests and for any future WrapClassFull caller.
//
// CrossCheck leaves the leg-2 fields ([CrossCheckResult.SACWrapped],
// SubsetBoundChecked, EscrowExcessStroops) and OverMintStroops at their
// zero values on purpose: under WrapClassFull the equality compare
// already SUBSUMES the escrow bound. SACWrapped ≤ classic.TotalSupply
// holds by construction (it is one of four non-negative addends), and
// this function only passes when classic.TotalSupply ≤
// sac.TotalSupply + [CrossCheckTolerance], so SACWrapped ≤
// sac.TotalSupply + tolerance follows for free. Evaluating leg 2
// separately here would add a second reading of a bound the equality
// has already proven.
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
		WrapClass:         WrapClassFull,
	}, nil
}

// CrossCheckSubsetBound compares a classic-asset Algorithm 2 reading
// with its SAC-wrapped Algorithm 3 reading under the [WrapClassPartial]
// invariants. It checks TWO conservation bounds, each of which is
// impossible to breach under correct accounting regardless of how much
// of the asset is wrapped:
//
//	leg 1 (over-mint, 2026-07-08 decision, BACKLOG #59)
//	    sac.TotalSupply ≤ classic.TotalSupply
//	  because Algorithm 2's total already includes the SAC-wrapped
//	  balance as one of its four non-negative addends (see
//	  [ClassicSupplyComponents]). OverMintStroops = the excess.
//
//	leg 2 (escrow-exceeds-minted, 2026-07-25, audit E4/N-F3(b))
//	    classic.SACWrappedStroops ≤ sac.TotalSupply
//	  because every unit escrowed inside the SAC got there by a mint,
//	  so the ledger-entry sum of SAC balances cannot exceed the
//	  event-derived net mint. EscrowExcessStroops = the excess.
//
// DivergenceStroops = max(OverMintStroops, EscrowExcessStroops), so the
// single existing gauge + alert fires on either breach with no
// threshold change, and the per-leg fields on [CrossCheckResult] say
// which one. Zero in the expected steady state for a partially-wrapped
// asset (leg 1's classic ≥ sac and leg 2's SACWrapped ≤ sac are both
// comfortably satisfied), so nothing pages.
//
// Leg 2 is CS-087-gated. When classic.SACWrappedStroops is nil the leg
// is NOT evaluated and SubsetBoundChecked stays false; it is never
// defaulted to zero, because 0 ≤ sac_total holds vacuously and a zero
// default would publish a green check that verified nothing. A caller
// reading WithinTolerance MUST read SubsetBoundChecked alongside it.
//
// STILL NOT A FULL RECONCILIATION (MNY-04, narrowed). What leg 2 closes
// is the direction that used to be structurally invisible: a mint the
// indexer never captured, or a burn it double-counted, now shows up as
// SACWrapped > sac_total instead of hiding inside the benign
// classic > sac gap. What remains open is the NON-SAC half of Algorithm
// 2 — trustline / claimable / LP-reserve balances have no independent
// second observation to reconcile against, so an undercount there is
// still invisible to this check (it merely widens the benign
// classic > sac gap). And leg 2 is only as strong as the SACWrapped
// component itself: an UNDER-counted SACWrapped — the documented
// BLND/EURC/KALE/PHO dormant-pool-balance case in [WrapClassPartial]'s
// KNOWN LIMITATION, cured by `supply seed-sac-balances -full-history` —
// sits BELOW sac_total and so passes leg 2 quietly. Leg 2 is an upper
// bound on escrow, not a proof that escrow was fully observed.
//
// Pure, same pre-conditions as [CrossCheck].
func CrossCheckSubsetBound(classic, sac Supply) (CrossCheckResult, error) {
	if classic.TotalSupply == nil || sac.TotalSupply == nil {
		return CrossCheckResult{}, ErrCrossCheckNilSupply
	}

	// Leg 1 (over-mint) is DIAGNOSTIC-ONLY since 2026-08-05: its
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
	// Both paged for a week as "divergence" while every unit was
	// accounted for. OverMintStroops is still computed and reported so
	// an operator can read the cumulative-vs-outstanding gap, but it
	// no longer feeds DivergenceStroops — leg 2 below is the bound
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
// safe default — via [normalizeWrapClass]. This is the entry point
// [CrossCheckRefresher] uses; CrossCheck / CrossCheckSubsetBound stay
// exported for direct unit testing and for callers that already know
// their class.
func CrossCheckForClass(classic, sac Supply, class WrapClass) (CrossCheckResult, error) {
	if normalizeWrapClass(class) == WrapClassFull {
		return CrossCheck(classic, sac)
	}
	return CrossCheckSubsetBound(classic, sac)
}
