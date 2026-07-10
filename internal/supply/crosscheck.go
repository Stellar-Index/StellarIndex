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
	// This is the "(c) completeness fallback" from the 2026-07-08
	// decision: the true subset compare — Algorithm 2's SACWrapped
	// component vs Algorithm 3's total_supply, which per the same
	// math IS a true equality (both measure the same wrapped amount
	// via independent data paths: a ledger-entry snapshot sum vs an
	// event-flow sum) — is NOT available at this compare site today.
	// [StorageClassicSupplyReader.ClassicSupplyAt] computes
	// SACWrapped as [ClassicSupplyComponents.SACWrapped] but
	// [ClassicComputer.Compute] folds it into TotalSupply before
	// returning a [Supply], and only the folded TotalSupply is
	// persisted to `asset_supply_history` — the table
	// [CrossCheckRefresher] reads via [SnapshotReader]. Wiring the
	// real subset compare would need either (1) a new
	// `sac_wrapped_stroops` column on `asset_supply_history` +
	// [Supply] field + [ClassicComputer.Compute] populating it, or
	// (2) the refresher querying [ClassicSupplyStore.SumSACBalancesAtOrBefore]
	// directly instead of reading the persisted snapshot. Both are
	// real schema/plumbing work, tracked as the (b) follow-up in
	// BACKLOG #59 — not implemented here to avoid faking the subset
	// with an approximation.
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
//   - [WrapClassPartial]: max(0, sac.TotalSupply − classic.TotalSupply)
//     — zero whenever sac_total ≤ classic_total (the expected state
//     for a partially-wrapped asset, including exact equality),
//     positive only when the SAC reports MORE than the classic total
//     could possibly back — the one direction that is a genuine
//     violation regardless of wrap fraction.
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
// subset-bound invariant (2026-07-08 decision, BACKLOG #59): the SAC's
// total_supply can never exceed the classic asset's total_supply,
// because Algorithm 2's total already includes the SAC-wrapped balance
// as one of its non-negative addends (see [ClassicSupplyComponents]).
//
// DivergenceStroops = max(0, sac.TotalSupply − classic.TotalSupply).
// Zero whenever the SAC total is within the classic total (the normal
// case for a partially-wrapped asset, and also for an exactly-fully-
// wrapped one) — WithinTolerance is then true and nothing pages.
// Positive only when the SAC reports more than the classic side could
// possibly back, which cannot happen under correct accounting and is
// therefore a genuine "escrow != minted" violation.
//
// Pure, same pre-conditions as [CrossCheck].
func CrossCheckSubsetBound(classic, sac Supply) (CrossCheckResult, error) {
	if classic.TotalSupply == nil || sac.TotalSupply == nil {
		return CrossCheckResult{}, ErrCrossCheckNilSupply
	}

	over := new(big.Int).Sub(sac.TotalSupply, classic.TotalSupply)
	if over.Sign() < 0 {
		over = big.NewInt(0)
	}

	return CrossCheckResult{
		ClassicKey:        classic.AssetKey,
		SACKey:            sac.AssetKey,
		ClassicTotal:      new(big.Int).Set(classic.TotalSupply),
		SACTotal:          new(big.Int).Set(sac.TotalSupply),
		DivergenceStroops: over,
		WithinTolerance:   over.Cmp(CrossCheckTolerance) <= 0,
		WrapClass:         WrapClassPartial,
	}, nil
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
